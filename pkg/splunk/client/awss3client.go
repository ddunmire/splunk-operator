// Copyright (c) 2018-2021 Splunk Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package client

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
)

// blank assignment to verify that AWSS3Client implements S3Client
var _ S3Client = &AWSS3Client{}
var gSess *session.Session

// SplunkAWSS3Client is an interface to AWS S3 client
type SplunkAWSS3Client interface {
	ListObjectsV2(options *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error)
}

// AWSS3Client is a client to implement S3 specific APIs
type AWSS3Client struct {
	Region             string
	BucketName         string
	AWSAccessKeyID     string
	AWSSecretAccessKey string
	Prefix             string
	StartAfter         string
	Endpoint           string
	Client             SplunkAWSS3Client
}

// regex to extract the region from the s3 endpoint
var regionRegex = ".*.s3[-,.](?P<region>.*).amazonaws.com"

// GetRegion extracts the region from the endpoint field
func GetRegion(endpoint string) string {
	pattern := regexp.MustCompile(regionRegex)
	return pattern.FindStringSubmatch(endpoint)[1]
}

// InitAWSClientWrapper is a wrapper around InitClientSession
func InitAWSClientWrapper(region, accessKeyID, secretAccessKey string) interface{} {
	return InitAWSClientSession(region, accessKeyID, secretAccessKey)
}

// InitAWSClientSession initializes and returns a client session object
func InitAWSClientSession(region, accessKeyID, secretAccessKey string) SplunkAWSS3Client {
	scopedLog := log.WithName("InitAWSClientSession")

	sess, err := session.NewSession(&aws.Config{
		Region: aws.String(region),
		Credentials: credentials.NewStaticCredentials(
			accessKeyID,     // id
			secretAccessKey, // secret
			""),
		MaxRetries: aws.Int(3),
	})
	if err != nil {
		scopedLog.Error(err, "Failed to initialize an AWS S3 session.")
		return nil
	}

	// TODO: Fix this and store it in the SplunkAWSS3Client
	gSess = sess

	return s3.New(sess)
}

// NewAWSS3Client returns an AWS S3 client
func NewAWSS3Client(bucketName string, accessKeyID string, secretAccessKey string, prefix string, startAfter string, endpoint string, fn GetInitFunc) (S3Client, error) {
	var s3SplunkClient SplunkAWSS3Client
	var err error
	region := GetRegion(endpoint)
	cl := fn(region, accessKeyID, secretAccessKey)
	if cl == nil {
		err = fmt.Errorf("Failed to create an AWS S3 client")
		return nil, err
	}

	s3SplunkClient = cl.(*s3.S3)

	return &AWSS3Client{
		Region:             region,
		BucketName:         bucketName,
		AWSAccessKeyID:     accessKeyID,
		AWSSecretAccessKey: secretAccessKey,
		Prefix:             prefix,
		StartAfter:         startAfter,
		Endpoint:           endpoint,
		Client:             s3SplunkClient,
	}, nil
}

// RegisterAWSS3Client will add the corresponding function pointer to the map
func RegisterAWSS3Client() {
	wrapperObject := GetS3ClientWrapper{GetS3Client: NewAWSS3Client, GetInitFunc: InitAWSClientWrapper}
	S3Clients["aws"] = wrapperObject
}

// GetAppsList get the list of apps from remote storage
func (awsclient *AWSS3Client) GetAppsList() (S3Response, error) {
	scopedLog := log.WithName("GetAppsList")

	scopedLog.Info("Getting Apps list", "AWS S3 Bucket", awsclient.BucketName)
	s3Resp := S3Response{}

	options := &s3.ListObjectsV2Input{
		Bucket:     aws.String(awsclient.BucketName),
		Prefix:     aws.String(awsclient.Prefix),
		StartAfter: aws.String(awsclient.StartAfter), // exclude the directory itself from listing
		MaxKeys:    aws.Int64(4000),                  // return upto 4K keys from S3
		Delimiter:  aws.String("/"),                  // limit the listing to 1 level only
	}

	client := awsclient.Client
	resp, err := client.ListObjectsV2(options)
	if err != nil {
		scopedLog.Error(err, "Unable to list items in bucket", "AWS S3 Bucket", awsclient.BucketName)
		return s3Resp, err
	}

	if resp.Contents == nil {
		err = fmt.Errorf("Empty objects list in the bucket: %s", awsclient.BucketName)
		return s3Resp, err
	}

	tmp, err := json.Marshal(resp.Contents)
	if err != nil {
		scopedLog.Error(err, "Failed to marshal s3 response", "AWS S3 Bucket", awsclient.BucketName)
		return s3Resp, err
	}

	err = json.Unmarshal(tmp, &(s3Resp.Objects))
	if err != nil {
		scopedLog.Error(err, "Failed to unmarshal s3 response", "AWS S3 Bucket", awsclient.BucketName)
		return s3Resp, err
	}

	return s3Resp, nil
}

// GetInitContainerImage returns the initContainer image to be used with this s3 client
func (awsclient *AWSS3Client) GetInitContainerImage() string {
	return ("amazon/aws-cli")
}

// GetInitContainerCmd returns the init container command on a per app source basis to be used by the initContainer
func (awsclient *AWSS3Client) GetInitContainerCmd(endpoint string, bucket string, path string, appSrcName string, appMnt string) []string {
	s3AppSrcPath := filepath.Join(bucket, path) + "/"
	podSyncPath := filepath.Join(appMnt, appSrcName) + "/"

	return ([]string{fmt.Sprintf("--endpoint-url=%s", endpoint), "s3", "sync", fmt.Sprintf("s3://%s", s3AppSrcPath), podSyncPath})
}

// DownloadFile downloads an app packages from remote storage
func (awsclient *AWSS3Client) DownloadFile(remoteFile string, localFile string) error {
	scopedLog := log.WithName("DownloadFile")

	file, err := os.Create(localFile)
	if err != nil {
		scopedLog.Error(err, "Unable to open file", "localFile", localFile)
		return err
	}
	defer file.Close()

	// TODO: Fix the sess get to make it from the AWSS3Client
	sess := gSess
	downloader := s3manager.NewDownloader(sess)

	numBytes, err := downloader.Download(file,
		&s3.GetObjectInput{
			Bucket: aws.String(awsclient.BucketName),
			Key:    aws.String(remoteFile),
		})
	if err != nil {
		scopedLog.Error(err, "Unable to download item %q, %v", remoteFile, err)
		return err
	}

	scopedLog.Info("File Downloaded", "remoteFile: ", remoteFile, "numBytes: ", numBytes, "localFile: ", localFile)

	return nil
}
