/*
 * Minio Go Library for Amazon S3 Compatible Cloud Storage (C) 2015 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package minio

import (
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BucketStatCh - bucket metadata struct sent over read channel.
type BucketStatCh struct {
	Stat BucketStat
	Err  error
}

// ObjectStatCh - object metadata struct sent over read channel.
type ObjectStatCh struct {
	Stat ObjectStat
	Err  error
}

// ObjectMultipartStatCh - multipart object metadata struct sent over read channel.
type ObjectMultipartStatCh struct {
	Stat ObjectMultipartStat
	Err  error
}

// BucketStat container for bucket metadata.
type BucketStat struct {
	// The name of the bucket.
	Name string
	// Date the bucket was created.
	CreationDate time.Time
}

// ObjectStat container for object metadata.
type ObjectStat struct {
	ETag         string
	Key          string
	LastModified time.Time
	Size         int64
	ContentType  string

	Owner struct {
		DisplayName string
		ID          string
	}

	// The class of storage used to store the object.
	StorageClass string
}

// ObjectMultipartStat container for multipart object metadata.
type ObjectMultipartStat struct {
	// Date and time at which the multipart upload was initiated.
	Initiated time.Time `type:"timestamp" timestampFormat:"iso8601"`

	Initiator initiator
	Owner     owner

	StorageClass string

	// Key of the object for which the multipart upload was initiated.
	Key  string
	Size int64

	// Upload ID that identifies the multipart upload.
	UploadID string `xml:"UploadId"`
}

// s3 region map used by bucket location constraint if necessary.
var regions = map[string]string{
	"s3-fips-us-gov-west-1.amazonaws.com": "us-gov-west-1",
	"s3.amazonaws.com":                    "us-east-1",
	"s3-external-1.amazonaws.com":         "us-east-1",
	"s3-us-west-1.amazonaws.com":          "us-west-1",
	"s3-us-west-2.amazonaws.com":          "us-west-2",
	"s3-eu-west-1.amazonaws.com":          "eu-west-1",
	"s3-eu-central-1.amazonaws.com":       "eu-central-1",
	"s3-ap-southeast-1.amazonaws.com":     "ap-southeast-1",
	"s3-ap-southeast-2.amazonaws.com":     "ap-southeast-2",
	"s3-ap-northeast-1.amazonaws.com":     "ap-northeast-1",
	"s3-sa-east-1.amazonaws.com":          "sa-east-1",
	"s3.cn-north-1.amazonaws.com.cn":      "cn-north-1",

	// Add google cloud storage as one of the regions
	"storage.googleapis.com": "google",
}

// getRegion returns a region based on its endpoint mapping.
func getRegion(host string) (region string) {
	if _, ok := regions[host]; ok {
		return regions[host]
	}
	// Region cannot be empty according to Amazon S3 for AWS Signature Version 4.
	// So we will in-turn address all the four quadrants of our galaxy.
	return "milkyway"
}

// SignatureType is type of Authorization requested for a given HTTP request.
type SignatureType int

// Different types of supported signatures - default is Latest i.e SignatureV4.
const (
	Latest SignatureType = iota
	SignatureV4
	SignatureV2
)

// isV2 - is signature SignatureV2?
func (s SignatureType) isV2() bool {
	return s == SignatureV2
}

// isV4 - is signature SignatureV4?
func (s SignatureType) isV4() bool {
	return s == SignatureV4
}

// isLatest - is signature Latest?
func (s SignatureType) isLatest() bool {
	return s == Latest
}

// Config - main configuration struct used to set endpoint, credentials, and other options for requests.
type Config struct {
	///  Standard options
	AccessKeyID     string        // AccessKeyID required for authorized requests.
	SecretAccessKey string        // SecretAccessKey required for authorized requests.
	Endpoint        string        // host endpoint eg:- https://s3.amazonaws.com
	Signature       SignatureType // choose a signature type if necessary.

	/// Advanced options
	// Optional field. If empty, region is determined automatically.
	// Set to override default behavior.
	Region string
	// Specify this to get server response in non XML style if server supports it
	AcceptType string

	/// Really Advanced options
	//
	// Set this to override default transport ``http.DefaultTransport``
	//
	// This transport is usually needed for debugging OR to add your own
	// custom TLS certificates on the client transport, for custom CA's and
	// certs which are not part of standard certificate authority follow this
	// example:-
	//
	//  tr := &http.Transport{
	//          TLSClientConfig:    &tls.Config{RootCAs: pool},
	//          DisableCompression: true,
	//  }
	//
	Transport http.RoundTripper

	/// Internal options
	// use SetUserAgent append to default, useful when minio-go is used with in your application
	userAgent      string
	isUserAgentSet bool // allow user agent's to be set only once
	isVirtualStyle bool // set when virtual hostnames are on
}

// Global constants
const (
	LibraryName    = "minio-go"
	LibraryVersion = "0.2.5"
)

// SetUserAgent - append to a default user agent
func (c *Config) SetUserAgent(name string, version string, comments ...string) {
	if c.isUserAgentSet {
		// if user agent already set do not set it
		return
	}
	// if no name and version is set we do not add new user agents
	if name != "" && version != "" {
		c.userAgent = c.userAgent + " " + name + "/" + version + " (" + strings.Join(comments, "; ") + ") "
		c.isUserAgentSet = true
	}
}

// API is a container which delegates methods that comply with CloudStorageAPI interface.
type API struct {
	apiCore
}

// New - instantiate minio client API with your input Config{}.
func New(config Config) (CloudStorageAPI, error) {
	if strings.TrimSpace(config.Region) == "" || len(config.Region) == 0 {
		u, err := url.Parse(config.Endpoint)
		if err != nil {
			return API{}, err
		}
		match, _ := filepath.Match("*.s3*.amazonaws.com", u.Host)
		if match {
			config.isVirtualStyle = true
			hostSplits := strings.SplitN(u.Host, ".", 2)
			u.Host = hostSplits[1]
		}
		matchGoogle, _ := filepath.Match("*.storage.googleapis.com", u.Host)
		if matchGoogle {
			config.isVirtualStyle = true
			hostSplits := strings.SplitN(u.Host, ".", 2)
			u.Host = hostSplits[1]
		}
		config.Region = getRegion(u.Host)
		if config.Region == "google" {
			// Google cloud storage is signature V2
			config.Signature = SignatureV2
		}
	}
	config.SetUserAgent(LibraryName, LibraryVersion, runtime.GOOS, runtime.GOARCH)
	config.isUserAgentSet = false // default
	return API{apiCore{&config}}, nil
}

// PresignedPostPolicy return POST form data that can be used for object upload.
func (a API) PresignedPostPolicy(p *PostPolicy) (map[string]string, error) {
	if p.expiration.IsZero() {
		return nil, errors.New("Expiration time must be specified")
	}
	if _, ok := p.formData["key"]; !ok {
		return nil, errors.New("object key must be specified")
	}
	if _, ok := p.formData["bucket"]; !ok {
		return nil, errors.New("bucket name must be specified")
	}
	return a.presignedPostPolicy(p), nil
}

/// Object operations.

/// Expires maximum is 7days - ie. 604800 and minimum is 1.

// PresignedPutObject get a presigned URL to upload an object.
func (a API) PresignedPutObject(bucket, object string, expires time.Duration) (string, error) {
	expireSeconds := int64(expires / time.Second)
	if expireSeconds < 1 || expireSeconds > 604800 {
		return "", invalidArgumentError("")
	}
	return a.presignedPutObject(bucket, object, expireSeconds)
}

// PresignedGetObject get a presigned URL to retrieve an object for third party apps.
func (a API) PresignedGetObject(bucket, object string, expires time.Duration) (string, error) {
	expireSeconds := int64(expires / time.Second)
	if expireSeconds < 1 || expireSeconds > 604800 {
		return "", invalidArgumentError("")
	}
	return a.presignedGetObject(bucket, object, expireSeconds, 0, 0)
}

// GetObject retrieve object. retrieves full object, if you need ranges use GetPartialObject.
func (a API) GetObject(bucket, object string) (io.ReadCloser, ObjectStat, error) {
	if err := invalidBucketError(bucket); err != nil {
		return nil, ObjectStat{}, err
	}
	if err := invalidObjectError(object); err != nil {
		return nil, ObjectStat{}, err
	}
	// get object
	return a.getObject(bucket, object, 0, 0)
}

// GetPartialObject retrieve partial object.
//
// Takes range arguments to download the specified range bytes of an object.
// Setting offset and length = 0 will download the full object.
// For more information about the HTTP Range header, go to http://www.w3.org/Protocols/rfc2616/rfc2616-sec14.html#sec14.35.
func (a API) GetPartialObject(bucket, object string, offset, length int64) (io.ReadCloser, ObjectStat, error) {
	if err := invalidBucketError(bucket); err != nil {
		return nil, ObjectStat{}, err
	}
	if err := invalidObjectError(object); err != nil {
		return nil, ObjectStat{}, err
	}
	// get partial object.
	return a.getObject(bucket, object, offset, length)
}

// completedParts is a wrapper to make parts sortable by their part numbers.
// multi part completion requires list of multi parts to be sorted.
type completedParts []completePart

func (a completedParts) Len() int           { return len(a) }
func (a completedParts) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a completedParts) Less(i, j int) bool { return a[i].PartNumber < a[j].PartNumber }

// minimumPartSize minimum part size per object after which PutObject behaves internally as multipart.
var minimumPartSize int64 = 1024 * 1024 * 5

// maxParts - maximum parts for a single multipart session.
var maxParts = int64(10000)

// maxPartSize - maximum part size for a single multipart upload operation.
var maxPartSize int64 = 1024 * 1024 * 1024 * 5

// maxConcurrentQueue - max concurrent upload queue.
var maxConcurrentQueue int64 = 4

// calculatePartSize - calculate the optimal part size for the given objectSize.
//
// NOTE: Assumption here is that for any given object upload to a S3 compatible object
// storage it will have the following parameters as constants.
//
//  maxParts
//  maximumPartSize
//  minimumPartSize
//
// if the partSize after division with maxParts is greater than minimumPartSize
// then choose miniumPartSize as the new part size, if not return minimumPartSize.
//
// special case where it happens to be that partSize is indeed bigger than the
// maximum part size just return maxPartSize.
func calculatePartSize(objectSize int64) int64 {
	// make sure last part has enough buffer and handle this poperly.
	partSize := (objectSize / (maxParts - 1))
	if partSize > minimumPartSize {
		if partSize > maxPartSize {
			return maxPartSize
		}
		return partSize
	}
	return minimumPartSize
}

// Initiate a fresh multipart upload
func (a API) newObjectUpload(bucket, object, contentType string, size int64, data io.Reader) error {
	initMultipartUploadResult, err := a.initiateMultipartUpload(bucket, object)
	if err != nil {
		return err
	}
	uploadID := initMultipartUploadResult.UploadID
	complMultipartUpload := completeMultipartUpload{}
	var totalLength int64

	// Calculate optimal part size for a given size.
	partSize := calculatePartSize(size)
	// Allocate bufferred error channel for maximum parts.
	errCh := make(chan error, maxParts)
	// Limit multi part queue size to concurrent.
	mpQueueCh := make(chan struct{}, maxConcurrentQueue)
	defer close(errCh)
	defer close(mpQueueCh)
	// Allocate a new wait group
	wg := new(sync.WaitGroup)

	for p := range chopper(data, partSize, nil) {
		// This check is primarily for last part.
		// This verifies if the part.Len was an unexpected read i.e if we lost few bytes.
		if p.Len < partSize && size > 0 {
			expectedPartLen := size - totalLength
			if expectedPartLen != p.Len {
				return ErrorResponse{
					Code:     "UnexpectedShortRead",
					Message:  "Data read ‘" + strconv.FormatInt(expectedPartLen, 10) + "’ is not equal to expected size ‘" + strconv.FormatInt(p.Len, 10) + "’",
					Resource: separator + bucket + separator + object,
				}
			}
		}
		// Limit to 4 parts a given time.
		mpQueueCh <- struct{}{}
		// Account for all parts uploaded simultaneousy.
		wg.Add(1)
		go func(errCh chan<- error, mpQueueCh <-chan struct{}, p piece) {
			defer wg.Done()
			defer func() {
				<-mpQueueCh
			}()
			if p.Err != nil {
				errCh <- p.Err
				return
			}
			var complPart completePart
			complPart, err = a.uploadPart(bucket, object, uploadID, p.MD5Sum, p.Num, p.Len, p.ReadSeeker)
			if err != nil {
				errCh <- err
				return
			}
			complMultipartUpload.Parts = append(complMultipartUpload.Parts, complPart)
			errCh <- nil
		}(errCh, mpQueueCh, p)
		totalLength += p.Len
	}
	wg.Wait()
	if err := <-errCh; err != nil {
		return err
	}
	sort.Sort(completedParts(complMultipartUpload.Parts))
	_, err = a.completeMultipartUpload(bucket, object, uploadID, complMultipartUpload)
	if err != nil {
		return err
	}
	return nil
}

// partMetadatCh - individual part metadata struct over read channel
type partMetadataCh struct {
	Metadata partMetadata
	Err      error
}

func (a API) listObjectPartsRecursive(bucket, object, uploadID string) <-chan partMetadataCh {
	partCh := make(chan partMetadataCh, 1000)
	go a.listObjectPartsRecursiveInRoutine(bucket, object, uploadID, partCh)
	return partCh
}

func (a API) listObjectPartsRecursiveInRoutine(bucket, object, uploadID string, ch chan<- partMetadataCh) {
	defer close(ch)
	listObjPartsResult, err := a.listObjectParts(bucket, object, uploadID, 0, 1000)
	if err != nil {
		ch <- partMetadataCh{
			Metadata: partMetadata{},
			Err:      err,
		}
		return
	}
	for _, uploadedPart := range listObjPartsResult.Parts {
		ch <- partMetadataCh{
			Metadata: uploadedPart,
			Err:      nil,
		}
	}
	for {
		if !listObjPartsResult.IsTruncated {
			break
		}
		listObjPartsResult, err = a.listObjectParts(bucket, object,
			uploadID, listObjPartsResult.NextPartNumberMarker, 1000)
		if err != nil {
			ch <- partMetadataCh{
				Metadata: partMetadata{},
				Err:      err,
			}
			return
		}
		for _, uploadedPart := range listObjPartsResult.Parts {
			ch <- partMetadataCh{
				Metadata: uploadedPart,
				Err:      nil,
			}
		}
	}
}

// getTotalMultipartSize - calculate total uploaded size for the a given multipart object.
func (a API) getTotalMultipartSize(bucket, object, uploadID string) (int64, error) {
	var size int64
	for part := range a.listObjectPartsRecursive(bucket, object, uploadID) {
		if part.Err != nil {
			return 0, part.Err
		}
		size += part.Metadata.Size
	}
	return size, nil
}

// continue previously interrupted multipart upload object at `uploadID`
func (a API) continueObjectUpload(bucket, object, uploadID string, size int64, data io.Reader) error {
	var skipPieces []skipPiece
	completeMultipartUpload := completeMultipartUpload{}
	var totalLength int64
	for part := range a.listObjectPartsRecursive(bucket, object, uploadID) {
		if part.Err != nil {
			return part.Err
		}
		var completedPart completePart
		completedPart.PartNumber = part.Metadata.PartNumber
		completedPart.ETag = part.Metadata.ETag
		completeMultipartUpload.Parts = append(completeMultipartUpload.Parts, completedPart)
		md5SumBytes, err := hex.DecodeString(strings.Trim(part.Metadata.ETag, "\"")) // trim off the odd double quotes
		if err != nil {
			return err
		}
		totalLength += part.Metadata.Size
		skipPieces = append(skipPieces, skipPiece{
			md5sum:      md5SumBytes,
			pieceNumber: part.Metadata.PartNumber,
		})
	}

	// Calculate the optimal part size for a given size.
	partSize := calculatePartSize(size)
	// Allocate bufferred error channel for maximum parts.
	errCh := make(chan error, maxParts)
	// Limit multipart queue size to maxConcurrentQueue.
	mpQueueCh := make(chan struct{}, maxConcurrentQueue)
	defer close(errCh)
	defer close(mpQueueCh)
	// Allocate a new wait group.
	wg := new(sync.WaitGroup)

	for p := range chopper(data, partSize, skipPieces) {
		// This check is primarily for last part.
		// This verifies if the part.Len was an unexpected read i.e if we lost few bytes.
		if p.Len < partSize && size > 0 {
			expectedPartLen := size - totalLength
			if expectedPartLen != p.Len {
				return ErrorResponse{
					Code:     "UnexpectedShortRead",
					Message:  "Data read ‘" + strconv.FormatInt(expectedPartLen, 10) + "’ is not equal to expected size ‘" + strconv.FormatInt(p.Len, 10) + "’",
					Resource: separator + bucket + separator + object,
				}
			}
		}
		// Limit to 4 parts a given time.
		mpQueueCh <- struct{}{}
		// Account for all parts uploaded simultaneousy.
		wg.Add(1)
		go func(errCh chan<- error, mpQueueCh <-chan struct{}, p piece) {
			defer wg.Done()
			defer func() {
				<-mpQueueCh
			}()
			if p.Err != nil {
				errCh <- p.Err
				return
			}
			completedPart, err := a.uploadPart(bucket, object, uploadID, p.MD5Sum, p.Num, p.Len, p.ReadSeeker)
			if err != nil {
				errCh <- err
				return
			}
			completeMultipartUpload.Parts = append(completeMultipartUpload.Parts, completedPart)
			errCh <- nil
		}(errCh, mpQueueCh, p)
		totalLength += p.Len
	}
	wg.Wait()
	if err := <-errCh; err != nil {
		return err
	}
	sort.Sort(completedParts(completeMultipartUpload.Parts))
	_, err := a.completeMultipartUpload(bucket, object, uploadID, completeMultipartUpload)
	if err != nil {
		return err
	}
	return nil
}

// PutObject create an object in a bucket.
//
// You must have WRITE permissions on a bucket to create an object.
//
// This version of PutObject automatically does multipart for more than 5MB worth of data.
func (a API) PutObject(bucket, object, contentType string, size int64, data io.Reader) error {
	if err := invalidBucketError(bucket); err != nil {
		return err
	}
	if err := invalidArgumentError(object); err != nil {
		return err
	}
	// for un-authenticated requests do not initiated multipart operation.
	//
	// NOTE: this behavior is only kept valid for S3, since S3 doesn't
	// allow unauthenticated multipart requests.
	if a.config.Region != "milkyway" {
		if a.config.AccessKeyID == "" || a.config.SecretAccessKey == "" {
			_, err := a.putObjectUnAuthenticated(bucket, object, contentType, size, data)
			if err != nil {
				return err
			}
			return nil
		}
	}
	// Special handling just for Google Cloud Storage.
	// TODO - we should remove this in future when we fully implement Resumable object upload.
	if a.config.Region == "google" {
		if size > maxPartSize {
			return ErrorResponse{
				Code:     "EntityTooLarge",
				Message:  "Your proposed upload exceeds the maximum allowed object size.",
				Resource: separator + bucket + separator + object,
			}
		}
		if _, err := a.putObject(bucket, object, contentType, nil, size, NewReadSeekCloser(data)); err != nil {
			return err
		}
		return nil
	}
	switch {
	case size < minimumPartSize && size > 0:
		// Single Part use case, use PutObject directly.
		for part := range chopper(data, minimumPartSize, nil) {
			if part.Err != nil {
				return part.Err
			}
			// This verifies if the part.Len was an unexpected read i.e if we lost few bytes
			if part.Len != size {
				return ErrorResponse{
					Code:     "MethodUnexpectedEOF",
					Message:  "Data read is less than the requested size",
					Resource: separator + bucket + separator + object,
				}
			}
			_, err := a.putObject(bucket, object, contentType, part.MD5Sum, part.Len, part.ReadSeeker)
			if err != nil {
				return err
			}
			return nil
		}
	default:
		var inProgress bool
		var inProgressUploadID string
		for mpUpload := range a.listMultipartUploadsRecursive(bucket, object) {
			if mpUpload.Err != nil {
				return mpUpload.Err
			}
			if mpUpload.Metadata.Key == object {
				inProgress = true
				inProgressUploadID = mpUpload.Metadata.UploadID
				break
			}
		}
		if !inProgress {
			return a.newObjectUpload(bucket, object, contentType, size, data)
		}
		return a.continueObjectUpload(bucket, object, inProgressUploadID, size, data)
	}
	return errors.New("Unexpected control flow, please report this error at https://github.com/minio/minio-go/issues")
}

// StatObject verify if object exists and you have permission to access it.
func (a API) StatObject(bucket, object string) (ObjectStat, error) {
	if err := invalidBucketError(bucket); err != nil {
		return ObjectStat{}, err
	}
	if err := invalidObjectError(object); err != nil {
		return ObjectStat{}, err
	}
	return a.headObject(bucket, object)
}

// RemoveObject remove an object from a bucket.
func (a API) RemoveObject(bucket, object string) error {
	if err := invalidBucketError(bucket); err != nil {
		return err
	}
	if err := invalidObjectError(object); err != nil {
		return err
	}
	return a.deleteObject(bucket, object)
}

/// Bucket operations

// MakeBucket makes a new bucket.
//
// Optional arguments are acl - by default all buckets are created
// with ``private`` acl.
//
// ACL valid values
//
//  private - owner gets full access [default].
//  public-read - owner gets full access, all others get read access.
//  public-read-write - owner gets full access, all others get full access too.
//  authenticated-read - owner gets full access, authenticated users get read access.
//
func (a API) MakeBucket(bucket string, acl BucketACL) error {
	if err := invalidBucketError(bucket); err != nil {
		return err
	}
	if !acl.isValidBucketACL() {
		return invalidArgumentError("")
	}
	location := a.config.Region
	if location == "milkyway" {
		location = ""
	}
	if location == "us-east-1" {
		location = ""
	}
	if location == "google" {
		location = ""
	}
	return a.putBucket(bucket, string(acl), location)
}

// SetBucketACL set the permissions on an existing bucket using access control lists (ACL).
//
// For example
//
//  private - owner gets full access [default].
//  public-read - owner gets full access, all others get read access.
//  public-read-write - owner gets full access, all others get full access too.
//  authenticated-read - owner gets full access, authenticated users get read access.
//
func (a API) SetBucketACL(bucket string, acl BucketACL) error {
	if err := invalidBucketError(bucket); err != nil {
		return err
	}
	if !acl.isValidBucketACL() {
		return invalidArgumentError("")
	}
	return a.putBucketACL(bucket, string(acl))
}

// GetBucketACL get the permissions on an existing bucket.
//
// Returned values are:
//
//  private - owner gets full access.
//  public-read - owner gets full access, others get read access.
//  public-read-write - owner gets full access, others get full access too.
//  authenticated-read - owner gets full access, authenticated users get read access.
//
func (a API) GetBucketACL(bucket string) (BucketACL, error) {
	if err := invalidBucketError(bucket); err != nil {
		return "", err
	}
	policy, err := a.getBucketACL(bucket)
	if err != nil {
		return "", err
	}
	grants := policy.AccessControlList.Grant
	switch {
	case len(grants) == 1:
		if grants[0].Grantee.URI == "" && grants[0].Permission == "FULL_CONTROL" {
			return BucketACL("private"), nil
		}
	case len(grants) == 2:
		for _, g := range grants {
			if g.Grantee.URI == "http://acs.amazonaws.com/groups/global/AuthenticatedUsers" && g.Permission == "READ" {
				return BucketACL("authenticated-read"), nil
			}
			if g.Grantee.URI == "http://acs.amazonaws.com/groups/global/AllUsers" && g.Permission == "READ" {
				return BucketACL("public-read"), nil
			}
		}
	case len(grants) == 3:
		for _, g := range grants {
			if g.Grantee.URI == "http://acs.amazonaws.com/groups/global/AllUsers" && g.Permission == "WRITE" {
				return BucketACL("public-read-write"), nil
			}
		}
	}
	return "", ErrorResponse{
		Code:      "NoSuchBucketPolicy",
		Message:   "The specified bucket does not have a bucket policy.",
		Resource:  "/" + bucket,
		RequestID: "minio",
	}
}

// BucketExists verify if bucket exists and you have permission to access it.
func (a API) BucketExists(bucket string) error {
	if err := invalidBucketError(bucket); err != nil {
		return err
	}
	return a.headBucket(bucket)
}

// RemoveBucket deletes the bucket named in the URI.
//
//  All objects (including all object versions and delete markers).
//  in the bucket must be deleted before successfully attempting this request.
func (a API) RemoveBucket(bucket string) error {
	if err := invalidBucketError(bucket); err != nil {
		return err
	}
	return a.deleteBucket(bucket)
}

type multiPartUploadCh struct {
	Metadata ObjectMultipartStat
	Err      error
}

func (a API) listMultipartUploadsRecursive(bucket, object string) <-chan multiPartUploadCh {
	ch := make(chan multiPartUploadCh, 1000)
	go a.listMultipartUploadsRecursiveInRoutine(bucket, object, ch)
	return ch
}

func (a API) listMultipartUploadsRecursiveInRoutine(bucket, object string, ch chan<- multiPartUploadCh) {
	defer close(ch)
	listMultipartUplResult, err := a.listMultipartUploads(bucket, "", "", object, "", 1000)
	if err != nil {
		ch <- multiPartUploadCh{
			Metadata: ObjectMultipartStat{},
			Err:      err,
		}
		return
	}
	for _, multiPartUpload := range listMultipartUplResult.Uploads {
		ch <- multiPartUploadCh{
			Metadata: multiPartUpload,
			Err:      nil,
		}
	}
	for {
		if !listMultipartUplResult.IsTruncated {
			break
		}
		listMultipartUplResult, err = a.listMultipartUploads(bucket,
			listMultipartUplResult.NextKeyMarker, listMultipartUplResult.NextUploadIDMarker, object, "", 1000)
		if err != nil {
			ch <- multiPartUploadCh{
				Metadata: ObjectMultipartStat{},
				Err:      err,
			}
			return
		}
		for _, multiPartUpload := range listMultipartUplResult.Uploads {
			ch <- multiPartUploadCh{
				Metadata: multiPartUpload,
				Err:      nil,
			}
		}
	}
}

// listIncompleteUploadsInRoutine is an internal goroutine function called for listing objects.
func (a API) listIncompleteUploadsInRoutine(bucket, prefix string, recursive bool, ch chan<- ObjectMultipartStatCh) {
	defer close(ch)
	if err := invalidBucketError(bucket); err != nil {
		ch <- ObjectMultipartStatCh{
			Stat: ObjectMultipartStat{},
			Err:  err,
		}
		return
	}
	switch {
	case recursive == true:
		var multipartMarker string
		var uploadIDMarker string
		for {
			result, err := a.listMultipartUploads(bucket, multipartMarker, uploadIDMarker, prefix, "", 1000)
			if err != nil {
				ch <- ObjectMultipartStatCh{
					Stat: ObjectMultipartStat{},
					Err:  err,
				}
				return
			}
			for _, objectSt := range result.Uploads {
				objectSt.Size, err = a.getTotalMultipartSize(bucket, objectSt.Key, objectSt.UploadID)
				if err != nil {
					ch <- ObjectMultipartStatCh{
						Stat: ObjectMultipartStat{},
						Err:  err,
					}
				}
				ch <- ObjectMultipartStatCh{
					Stat: objectSt,
					Err:  nil,
				}
				multipartMarker = result.NextKeyMarker
				uploadIDMarker = result.NextUploadIDMarker
			}
			if !result.IsTruncated {
				break
			}
		}
	default:
		var multipartMarker string
		var uploadIDMarker string
		for {
			result, err := a.listMultipartUploads(bucket, multipartMarker, uploadIDMarker, prefix, "/", 1000)
			if err != nil {
				ch <- ObjectMultipartStatCh{
					Stat: ObjectMultipartStat{},
					Err:  err,
				}
				return
			}
			multipartMarker = result.NextKeyMarker
			uploadIDMarker = result.NextUploadIDMarker
			for _, objectSt := range result.Uploads {
				objectSt.Size, err = a.getTotalMultipartSize(bucket, objectSt.Key, objectSt.UploadID)
				if err != nil {
					ch <- ObjectMultipartStatCh{
						Stat: ObjectMultipartStat{},
						Err:  err,
					}
				}
				ch <- ObjectMultipartStatCh{
					Stat: objectSt,
					Err:  nil,
				}
			}
			for _, prefix := range result.CommonPrefixes {
				object := ObjectMultipartStat{}
				object.Key = prefix.Prefix
				object.Size = 0
				ch <- ObjectMultipartStatCh{
					Stat: object,
					Err:  nil,
				}
			}
			if !result.IsTruncated {
				break
			}
		}
	}
}

// ListIncompleteUploads - List incompletely uploaded multipart objects.
//
// ListIncompleteUploads is a channel based API implemented to facilitate ease of usage of S3 API ListMultipartUploads()
// by automatically recursively traversing all multipart objects on a given bucket if specified.
//
// Your input paramters are just bucket, prefix and recursive.
// If you enable recursive as 'true' this function will return back all the multipart objects in a given bucket.
//
//   api := client.New(....)
//   recursive := true
//   for message := range api.ListIncompleteUploads("mytestbucket", "starthere", recursive) {
//       fmt.Println(message.Stat)
//   }
//
func (a API) ListIncompleteUploads(bucket, prefix string, recursive bool) <-chan ObjectMultipartStatCh {
	ch := make(chan ObjectMultipartStatCh, 1000)
	go a.listIncompleteUploadsInRoutine(bucket, prefix, recursive, ch)
	return ch
}

// listObjectsInRoutine is an internal goroutine function called for listing objects.
// This function feeds data into channel.
func (a API) listObjectsInRoutine(bucket, prefix string, recursive bool, ch chan<- ObjectStatCh) {
	defer close(ch)
	if err := invalidBucketError(bucket); err != nil {
		ch <- ObjectStatCh{
			Stat: ObjectStat{},
			Err:  err,
		}
		return
	}
	switch {
	case recursive == true:
		var marker string
		for {
			result, err := a.listObjects(bucket, marker, prefix, "", 1000)
			if err != nil {
				ch <- ObjectStatCh{
					Stat: ObjectStat{},
					Err:  err,
				}
				return
			}
			for _, object := range result.Contents {
				ch <- ObjectStatCh{
					Stat: object,
					Err:  nil,
				}
				marker = object.Key
			}
			if !result.IsTruncated {
				break
			}
		}
	default:
		var marker string
		for {
			result, err := a.listObjects(bucket, marker, prefix, "/", 1000)
			if err != nil {
				ch <- ObjectStatCh{
					Stat: ObjectStat{},
					Err:  err,
				}
				return
			}
			marker = result.NextMarker
			for _, object := range result.Contents {
				ch <- ObjectStatCh{
					Stat: object,
					Err:  nil,
				}
			}
			for _, prefix := range result.CommonPrefixes {
				object := ObjectStat{}
				object.Key = prefix.Prefix
				object.Size = 0
				ch <- ObjectStatCh{
					Stat: object,
					Err:  nil,
				}
			}
			if !result.IsTruncated {
				break
			}
		}
	}
}

// ListObjects - (List Objects) - List some objects or all recursively.
//
// ListObjects is a channel based API implemented to facilitate ease of usage of S3 API ListObjects()
// by automatically recursively traversing all objects on a given bucket if specified.
//
// Your input paramters are just bucket, prefix and recursive.
// If you enable recursive as 'true' this function will return back all the objects in a given bucket.
//
//   api := client.New(....)
//   recursive := true
//   for message := range api.ListObjects("mytestbucket", "starthere", recursive) {
//       fmt.Println(message.Stat)
//   }
//
func (a API) ListObjects(bucket string, prefix string, recursive bool) <-chan ObjectStatCh {
	ch := make(chan ObjectStatCh, 1000)
	go a.listObjectsInRoutine(bucket, prefix, recursive, ch)
	return ch
}

// listBucketsInRoutine is an internal go routine function called for listing buckets
// This function feeds data into channel
func (a API) listBucketsInRoutine(ch chan<- BucketStatCh) {
	defer close(ch)
	listAllMyBucketListResults, err := a.listBuckets()
	if err != nil {
		ch <- BucketStatCh{
			Stat: BucketStat{},
			Err:  err,
		}
		return
	}
	for _, bucket := range listAllMyBucketListResults.Buckets.Bucket {
		ch <- BucketStatCh{
			Stat: bucket,
			Err:  nil,
		}
	}
}

// ListBuckets list of all buckets owned by the authenticated sender of the request.
//
//   This call requires explicit authentication, no anonymous
//   requests are allowed for listing buckets
//
//   api := client.New(....)
//   for message := range api.ListBuckets() {
//       fmt.Println(message.Stat)
//   }
//
func (a API) ListBuckets() <-chan BucketStatCh {
	ch := make(chan BucketStatCh, 100)
	go a.listBucketsInRoutine(ch)
	return ch
}

func (a API) removeIncompleteUploadInRoutine(bucket, object string, errorCh chan<- error) {
	defer close(errorCh)
	if err := invalidBucketError(bucket); err != nil {
		errorCh <- err
		return
	}
	listMultipartUplResult, err := a.listMultipartUploads(bucket, "", "", object, "", 1000)
	if err != nil {
		errorCh <- err
		return
	}
	for _, multiPartUpload := range listMultipartUplResult.Uploads {
		if object == multiPartUpload.Key {
			err := a.abortMultipartUpload(bucket, multiPartUpload.Key, multiPartUpload.UploadID)
			if err != nil {
				errorCh <- err
				return
			}
			return
		}
	}
	for {
		if !listMultipartUplResult.IsTruncated {
			break
		}
		listMultipartUplResult, err = a.listMultipartUploads(bucket,
			listMultipartUplResult.NextKeyMarker, listMultipartUplResult.NextUploadIDMarker, object, "", 1000)
		if err != nil {
			errorCh <- err
			return
		}
		for _, multiPartUpload := range listMultipartUplResult.Uploads {
			if object == multiPartUpload.Key {
				err := a.abortMultipartUpload(bucket, multiPartUpload.Key, multiPartUpload.UploadID)
				if err != nil {
					errorCh <- err
					return
				}
				return
			}
		}

	}
}

// RemoveIncompleteUpload - abort a specific in progress active multipart upload
//   requires explicit authentication, no anonymous requests are allowed for multipart API
func (a API) RemoveIncompleteUpload(bucket, object string) <-chan error {
	errorCh := make(chan error)
	go a.removeIncompleteUploadInRoutine(bucket, object, errorCh)
	return errorCh
}