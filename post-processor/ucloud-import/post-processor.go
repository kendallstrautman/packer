//go:generate packer-sdc mapstructure-to-hcl2 -type Config
//go:generate packer-sdc struct-markdown

package ucloudimport

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2/hcldec"
	"github.com/hashicorp/packer-plugin-sdk/common"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hashicorp/packer-plugin-sdk/retry"
	"github.com/hashicorp/packer-plugin-sdk/template/config"
	"github.com/hashicorp/packer-plugin-sdk/template/interpolate"
	ucloudcommon "github.com/hashicorp/packer/builder/ucloud/common"
	"github.com/ucloud/ucloud-sdk-go/services/ufile"
	"github.com/ucloud/ucloud-sdk-go/services/uhost"
	"github.com/ucloud/ucloud-sdk-go/ucloud"
	ufsdk "github.com/ufilesdk-dev/ufile-gosdk"
)

const (
	BuilderId = "packer.post-processor.ucloud-import"

	ImageFileFormatRAW   = "raw"
	ImageFileFormatVHD   = "vhd"
	ImageFileFormatVMDK  = "vmdk"
	ImageFileFormatQCOW2 = "qcow2"
)

var imageFormatMap = ucloudcommon.NewStringConverter(map[string]string{
	"raw":  "RAW",
	"vhd":  "VHD",
	"vmdk": "VMDK",
})

// Configuration of this post processor
type Config struct {
	common.PackerConfig       `mapstructure:",squash"`
	ucloudcommon.AccessConfig `mapstructure:",squash"`

	//  The name of the UFile bucket where the RAW, VHD, VMDK, or qcow2 file will be copied to for import.
	//  This bucket must exist when the post-processor is run.
	UFileBucket string `mapstructure:"ufile_bucket_name" required:"true"`
	// The name of the object key in
	//  `ufile_bucket_name` where the RAW, VHD, VMDK, or qcow2 file will be copied
	//  to import. This is a [template engine](/docs/templates/legacy_json_templates/engine).
	//  Therefore, you may use user variables and template functions in this field.
	UFileKey string `mapstructure:"ufile_key_name" required:"false"`
	// Whether we should skip removing the RAW, VHD, VMDK, or qcow2 file uploaded to
	// UFile after the import process has completed. Possible values are: `true` to
	// leave it in the UFile bucket, `false` to remove it. (Default: `false`).
	SkipClean bool `mapstructure:"skip_clean" required:"false"`
	// The name of the user-defined image, which contains 1-63 characters and only
	// supports Chinese, English, numbers, '-\_,.:[]'.
	ImageName string `mapstructure:"image_name" required:"true"`
	// The description of the image.
	ImageDescription string `mapstructure:"image_description" required:"false"`
	// Type of the OS. Possible values are: `CentOS`, `Ubuntu`, `Windows`, `RedHat`, `Debian`, `Other`.
	// You may refer to [ucloud_api_docs](https://docs.ucloud.cn/api/uhost-api/import_custom_image) for detail.
	OSType string `mapstructure:"image_os_type" required:"true"`
	// The name of OS. Such as: `CentOS 7.2 64位`, set `Other` When `image_os_type` is `Other`.
	// You may refer to [ucloud_api_docs](https://docs.ucloud.cn/api/uhost-api/import_custom_image) for detail.
	OSName string `mapstructure:"image_os_name" required:"true"`
	// The format of the import image , Possible values are: `raw`, `vhd`, `vmdk`, or `qcow2`.
	Format string `mapstructure:"format" required:"true"`
	// Timeout of importing image. The default timeout is 3600 seconds if this option is not set or is set.
	WaitImageReadyTimeout int `mapstructure:"wait_image_ready_timeout" required:"false"`

	ctx interpolate.Context
}

type PostProcessor struct {
	config Config
}

func (p *PostProcessor) ConfigSpec() hcldec.ObjectSpec { return p.config.FlatMapstructure().HCL2Spec() }

func (p *PostProcessor) Configure(raws ...interface{}) error {
	err := config.Decode(&p.config, &config.DecodeOpts{
		PluginType:         BuilderId,
		Interpolate:        true,
		InterpolateContext: &p.config.ctx,
		InterpolateFilter: &interpolate.RenderFilter{
			Exclude: []string{
				"ufile_key_name",
			},
		},
	}, raws...)
	if err != nil {
		return err
	}

	// Set defaults
	if p.config.UFileKey == "" {
		p.config.UFileKey = "packer-import-{{timestamp}}." + p.config.Format
	}

	if p.config.WaitImageReadyTimeout <= 0 {
		p.config.WaitImageReadyTimeout = ucloudcommon.DefaultCreateImageTimeout
	}

	errs := new(packersdk.MultiError)

	// Check and render ufile_key_name
	if err = interpolate.Validate(p.config.UFileKey, &p.config.ctx); err != nil {
		errs = packersdk.MultiErrorAppend(
			errs, fmt.Errorf("Error parsing ufile_key_name template: %s", err))
	}

	// Check we have ucloud access variables defined somewhere
	errs = packersdk.MultiErrorAppend(errs, p.config.AccessConfig.Prepare(&p.config.ctx)...)

	// define all our required parameters
	templates := map[string]*string{
		"ufile_bucket_name": &p.config.UFileBucket,
		"image_name":        &p.config.ImageName,
		"image_os_type":     &p.config.OSType,
		"image_os_name":     &p.config.OSName,
		"format":            &p.config.Format,
	}
	// Check out required params are defined
	for key, ptr := range templates {
		if *ptr == "" {
			errs = packersdk.MultiErrorAppend(
				errs, fmt.Errorf("%s must be set", key))
		}
	}

	imageName := p.config.ImageName
	if !ucloudcommon.ImageNamePattern.MatchString(imageName) {
		errs = packersdk.MultiErrorAppend(errs, fmt.Errorf("expected %q to be 1-63 characters and only support chinese, english, numbers, '-_,.:[]', got %q", "image_name", imageName))
	}

	switch p.config.Format {
	case ImageFileFormatVHD, ImageFileFormatRAW, ImageFileFormatVMDK, ImageFileFormatQCOW2:
	default:
		errs = packersdk.MultiErrorAppend(
			errs, fmt.Errorf("expected %q only be one of 'raw', 'vhd', 'vmdk', or 'qcow2', got %q", "format", p.config.Format))
	}

	// Anything which flagged return back up the stack
	if len(errs.Errors) > 0 {
		return errs
	}

	packersdk.LogSecretFilter.Set(p.config.PublicKey, p.config.PrivateKey)
	log.Println(p.config)
	return nil
}

func (p *PostProcessor) PostProcess(ctx context.Context, ui packersdk.Ui, artifact packersdk.Artifact) (packersdk.Artifact, bool, bool, error) {
	var err error

	generatedData := artifact.State("generated_data")
	if generatedData == nil {
		// Make sure it's not a nil map so we can assign to it later.
		generatedData = make(map[string]interface{})
	}
	p.config.ctx.Data = generatedData

	client, err := p.config.Client()
	if err != nil {
		return nil, false, false, fmt.Errorf("Failed to connect ucloud client %s", err)
	}
	uhostconn := client.UHostConn
	ufileconn := client.UFileConn

	// Render this key since we didn't in the configure phase
	p.config.UFileKey, err = interpolate.Render(p.config.UFileKey, &p.config.ctx)
	if err != nil {
		return nil, false, false, fmt.Errorf("Error rendering ufile_key_name template: %s", err)
	}

	ui.Message(fmt.Sprintf("Rendered ufile_key_name as %s", p.config.UFileKey))

	ui.Message("Looking for image in artifact")
	// Locate the files output from the builder
	var source string
	for _, path := range artifact.Files() {
		if strings.HasSuffix(path, "."+p.config.Format) {
			source = path
			break
		}
	}

	// Hope we found something useful
	if source == "" {
		return nil, false, false, fmt.Errorf("No %s image file found in artifact from builder", p.config.Format)
	}

	keyName := p.config.UFileKey
	bucketName := p.config.UFileBucket

	// query bucket
	domain, err := queryBucket(ufileconn, bucketName)
	if err != nil {
		return nil, false, false, fmt.Errorf("Failed to query bucket, %s", err)
	}

	var bucketHost string
	if p.config.BaseUrl != "" {
		// skip error because it has been validated by prepare
		urlObj, _ := url.Parse(p.config.BaseUrl)
		bucketHost = urlObj.Host
	} else {
		bucketHost = "api.ucloud.cn"
	}

	fileHost := strings.SplitN(domain, ".", 2)[1]

	config := &ufsdk.Config{
		PublicKey:  p.config.PublicKey,
		PrivateKey: p.config.PrivateKey,
		BucketName: bucketName,
		FileHost:   fileHost,
		BucketHost: bucketHost,
	}

	ui.Say(fmt.Sprintf("Waiting for uploading image file %s to UFile: %s/%s...", source, bucketName, keyName))

	// upload file to bucket
	ufileUrl, err := uploadFile(ufileconn, config, keyName, source)
	if err != nil {
		return nil, false, false, fmt.Errorf("Failed to Upload image file, %s", err)
	}

	ui.Say(fmt.Sprintf("Image file %s has been uploaded to UFile: %s/%s", source, bucketName, keyName))

	importImageRequest := p.buildImportImageRequest(uhostconn, ufileUrl)
	importImageResponse, err := uhostconn.ImportCustomImage(importImageRequest)
	if err != nil {
		return nil, false, false, fmt.Errorf("Failed to import image from UFile: %s/%s, %s", bucketName, keyName, err)
	}

	ui.Say(fmt.Sprintf("Waiting for importing image from UFile: %s/%s ...", bucketName, keyName))

	imageId := importImageResponse.ImageId
	err = retry.Config{
		StartTimeout: time.Duration(p.config.WaitImageReadyTimeout) * time.Second,
		ShouldRetry: func(err error) bool {
			return ucloudcommon.IsExpectedStateError(err)
		},
		RetryDelay: (&retry.Backoff{InitialBackoff: 2 * time.Second, MaxBackoff: 12 * time.Second, Multiplier: 2}).Linear,
	}.Run(ctx, func(ctx context.Context) error {
		image, err := client.DescribeImageById(imageId)
		if err != nil {
			return err
		}

		if image.State == ucloudcommon.ImageStateUnavailable {
			return fmt.Errorf("Unavailable importing image %q", imageId)
		}

		if image.State != ucloudcommon.ImageStateAvailable {
			return ucloudcommon.NewExpectedStateError("image", imageId)
		}

		return nil
	})

	if err != nil {
		return nil, false, false, fmt.Errorf("Error on waiting for importing image %q from UFile: %s/%s, %s",
			imageId, bucketName, keyName, err)
	}

	// Add the reported UCloud image ID to the artifact list
	ui.Say(fmt.Sprintf("Importing created ucloud image %q in region %q Complete.", imageId, p.config.Region))
	images := []ucloudcommon.ImageInfo{
		{
			ImageId:   imageId,
			ProjectId: p.config.ProjectId,
			Region:    p.config.Region,
		},
	}

	artifact = &ucloudcommon.Artifact{
		UCloudImages:   ucloudcommon.NewImageInfoSet(images),
		BuilderIdValue: BuilderId,
		Client:         client,
	}

	if !p.config.SkipClean {
		ui.Message(fmt.Sprintf("Deleting import source UFile: %s/%s", p.config.UFileBucket, p.config.UFileKey))
		if err = deleteFile(config, p.config.UFileKey); err != nil {
			return nil, false, false, fmt.Errorf("Failed to delete UFile: %s/%s, %s", p.config.UFileBucket, p.config.UFileKey, err)
		}
	}

	return artifact, false, false, nil
}

func (p *PostProcessor) buildImportImageRequest(conn *uhost.UHostClient, privateUrl string) *uhost.ImportCustomImageRequest {
	req := conn.NewImportCustomImageRequest()
	req.ImageName = ucloud.String(p.config.ImageName)
	req.ImageDescription = ucloud.String(p.config.ImageDescription)
	req.UFileUrl = ucloud.String(privateUrl)
	req.OsType = ucloud.String(p.config.OSType)
	req.OsName = ucloud.String(p.config.OSName)
	req.Format = ucloud.String(imageFormatMap.Convert(p.config.Format))
	req.Auth = ucloud.Bool(true)
	return req
}

func queryBucket(conn *ufile.UFileClient, bucketName string) (string, error) {
	req := conn.NewDescribeBucketRequest()
	req.BucketName = ucloud.String(bucketName)
	resp, err := conn.DescribeBucket(req)
	if err != nil {
		return "", fmt.Errorf("error on reading bucket %q when create bucket, %s", bucketName, err)
	}

	if len(resp.DataSet) < 1 {
		return "", fmt.Errorf("the bucket %s is not exit", bucketName)
	}

	return resp.DataSet[0].Domain.Src[0], nil
}

func uploadFile(conn *ufile.UFileClient, config *ufsdk.Config, keyName, source string) (string, error) {
	reqFile, err := ufsdk.NewFileRequest(config, nil)
	if err != nil {
		return "", fmt.Errorf("error on building upload file request, %s", err)
	}

	// upload file in segments
	err = reqFile.AsyncMPut(source, keyName, "")
	if err != nil {
		return "", fmt.Errorf("error on upload file, %s, details: %s", err, reqFile.DumpResponse(true))
	}

	reqBucket := conn.NewDescribeBucketRequest()
	reqBucket.BucketName = ucloud.String(config.BucketName)
	resp, err := conn.DescribeBucket(reqBucket)
	if err != nil {
		return "", fmt.Errorf("error on reading bucket list when upload file, %s", err)
	}

	if resp.DataSet[0].Type == "private" {
		return reqFile.GetPrivateURL(keyName, time.Duration(24*60*60)*time.Second), nil
	}

	return reqFile.GetPublicURL(keyName), nil
}

func deleteFile(config *ufsdk.Config, keyName string) error {
	req, err := ufsdk.NewFileRequest(config, nil)
	if err != nil {
		return fmt.Errorf("error on new deleting file, %s", err)
	}
	req.DeleteFile(keyName)
	if err != nil {
		return fmt.Errorf("error on deleting file, %s", err)
	}

	return nil
}
