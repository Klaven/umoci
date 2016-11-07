/*
 * umoci: Umoci Modifies Open Containers' Images
 * Copyright (C) 2016 SUSE LLC.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cas

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"

	"github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/net/context"
)

// ImageLayout is the structure in the "oci-layout" file, found in the root
// of an OCI Image-layout directory.
// XXX: This comes from the spec, but hasn't been vendored.
type ImageLayout struct {
	Version string `json:"imageLayoutVersion"`
}

// ImageLayoutVersion is the version of the image layout we support. This value
// is *not* the same as imagespec.Version, and the meaning of this field is
// still under discussion in the spec. For now we'll just hardcode the value
// and hope for the best.
const ImageLayoutVersion = "1.0.0"

// CreateLayout creates a new OCI image layout at the given path. If the path
// already exists, os.ErrExist is returned. However, all of the parent
// components of the path will be created if necessary.
func CreateLayout(path string) error {
	// We need to fail if path already exists, but we first create all of the
	// parent paths.
	dir := filepath.Dir(path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	if err := os.Mkdir(path, 0755); err != nil {
		return err
	}

	// Create the necessary directories and "oci-layout" file.
	if err := os.Mkdir(filepath.Join(path, blobDirectory), 0755); err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(path, refDirectory), 0755); err != nil {
		return err
	}

	fh, err := os.Create(filepath.Join(path, layoutFile))
	if err != nil {
		return err
	}
	defer fh.Close()

	ociLayout := &ImageLayout{
		Version: ImageLayoutVersion,
	}

	if err := json.NewEncoder(fh).Encode(ociLayout); err != nil {
		return err
	}

	// Everything is now set up.
	return nil
}

type dirEngine struct {
	path string
	temp string
}

// verify ensures that the image is valid.
// FIXME: Actually validate the full image, not just the layoutFile.
// FIXME: Return more information about what was invalid about the image.
func (e dirEngine) validate() error {
	content, err := ioutil.ReadFile(filepath.Join(e.path, layoutFile))
	if err != nil {
		if os.IsNotExist(err) {
			err = ErrInvalid
		}
		return err
	}

	var ociLayout ImageLayout
	if err := json.Unmarshal(content, &ociLayout); err != nil {
		return err
	}

	// XXX: Currently the meaning of this field is not adequately defined by
	//      the spec, nor is the "official" value determined by the spec.
	if ociLayout.Version != ImageLayoutVersion {
		return ErrInvalid
	}

	// Check that "blobs" and "refs" exist in the image.
	// FIXME: We also should check that blobs *only* contains a BlobAlgorithm
	//        directory (with no subdirectories) and that refs *only* contains
	//        files (optionally also making sure they're all JSON descriptors).
	if fi, err := os.Stat(filepath.Join(e.path, blobDirectory)); err != nil {
		if os.IsNotExist(err) {
			err = ErrInvalid
		}
		return err
	} else if !fi.IsDir() {
		return ErrInvalid
	}

	if fi, err := os.Stat(filepath.Join(e.path, refDirectory)); err != nil {
		if os.IsNotExist(err) {
			err = ErrInvalid
		}
		return err
	} else if !fi.IsDir() {
		return ErrInvalid
	}

	return nil
}

// PutBlob adds a new blob to the image. This is idempotent; a nil error
// means that "the content is stored at DIGEST" without implying "because
// of this PutBlob() call".
func (e dirEngine) PutBlob(ctx context.Context, reader io.Reader) (string, int64, error) {
	hash := sha256.New()
	algo := BlobAlgorithm

	// We copy this into a temporary file because we need to get the blob hash,
	// but also to avoid half-writing an invalid blob.
	fh, err := ioutil.TempFile(e.temp, "blob-")
	if err != nil {
		return "", -1, err
	}
	tempPath := fh.Name()
	defer fh.Close()

	writer := io.MultiWriter(fh, hash)
	size, err := io.Copy(writer, reader)
	if err != nil {
		return "", -1, err
	}
	fh.Close()

	// Get the digest.
	digest := fmt.Sprintf("%s:%x", algo, hash.Sum(nil))
	path, err := blobPath(digest)
	if err != nil {
		return "", -1, err
	}

	// Move the blob to its correct path.
	path = filepath.Join(e.path, path)
	if err := os.Rename(tempPath, path); err != nil {
		return "", -1, err
	}

	return digest, int64(size), nil
}

// PutBlobJSON adds a new JSON blob to the image (marshalled from the given
// interface). This is equivalent to calling PutBlob() with a JSON payload
// as the reader. Note that due to intricacies in the Go JSON
// implementation, we cannot guarantee that two calls to PutBlobJSON() will
// return the same digest.
func (e dirEngine) PutBlobJSON(ctx context.Context, data interface{}) (string, int64, error) {
	var buffer bytes.Buffer
	if err := json.NewEncoder(&buffer).Encode(data); err != nil {
		return "", -1, err
	}
	return e.PutBlob(ctx, &buffer)
}

// PutReference adds a new reference descriptor blob to the image. This is
// idempotent; a nil error means that "the descriptor is stored at NAME"
// without implying "because of this PutReference() call". ErrClobber is
// returned if there is already a descriptor stored at NAME, but does not
// match the descriptor requested to be stored.
func (e dirEngine) PutReference(ctx context.Context, name string, descriptor *v1.Descriptor) error {

	if oldDescriptor, err := e.GetReference(ctx, name); err == nil {
		// We should not return an error if the two descriptors are identical.
		if !reflect.DeepEqual(oldDescriptor, descriptor) {
			return ErrClobber
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	// We copy this into a temporary file to avoid half-writing an invalid
	// reference.
	fh, err := ioutil.TempFile(e.temp, "ref."+name+"-")
	if err != nil {
		return err
	}
	tempPath := fh.Name()
	defer fh.Close()

	// Write out descriptor.
	if err := json.NewEncoder(fh).Encode(descriptor); err != nil {
		return err
	}
	fh.Close()

	path, err := refPath(name)
	if err != nil {
		return err
	}

	// Move the ref to its correct path.
	path = filepath.Join(e.path, path)
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}

	return nil
}

// GetBlob returns a reader for retrieving a blob from the image, which the
// caller must Close(). Returns os.ErrNotExist if the digest is not found.
func (e dirEngine) GetBlob(ctx context.Context, digest string) (io.ReadCloser, error) {
	path, err := blobPath(digest)
	if err != nil {
		return nil, err
	}
	return os.Open(filepath.Join(e.path, path))
}

// GetReference returns a reference from the image. Returns os.ErrNotExist
// if the name was not found.
func (e dirEngine) GetReference(ctx context.Context, name string) (*v1.Descriptor, error) {
	path, err := refPath(name)
	if err != nil {
		return nil, err
	}

	content, err := ioutil.ReadFile(filepath.Join(e.path, path))
	if err != nil {
		return nil, err
	}

	var descriptor v1.Descriptor
	if err := json.Unmarshal(content, &descriptor); err != nil {
		return nil, err
	}

	// XXX: Do we need to validate the descriptor?
	return &descriptor, nil
}

// DeleteBlob removes a blob from the image. This is idempotent; a nil
// error means "the content is not in the store" without implying "because
// of this DeleteBlob() call".
func (e dirEngine) DeleteBlob(ctx context.Context, digest string) error {
	path, err := blobPath(digest)
	if err != nil {
		return err
	}

	err = os.Remove(filepath.Join(e.path, path))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// DeleteReference removes a reference from the image. This is idempotent;
// a nil error means "the content is not in the store" without implying
// "because of this DeleteReference() call".
func (e dirEngine) DeleteReference(ctx context.Context, name string) error {
	path, err := refPath(name)
	if err != nil {
		return err
	}

	err = os.Remove(filepath.Join(e.path, path))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ListBlobs returns the set of blob digests stored in the image.
func (e dirEngine) ListBlobs(ctx context.Context) ([]string, error) {
	digests := []string{}
	blobDir := filepath.Join(e.path, blobDirectory, BlobAlgorithm)

	if err := filepath.Walk(blobDir, func(path string, _ os.FileInfo, _ error) error {
		// Skip the actual directory.
		if path == blobDir {
			return nil
		}

		// XXX: Do we need to handle multiple-directory-deep cases?
		digest := fmt.Sprintf("%s:%s", BlobAlgorithm, filepath.Base(path))
		digests = append(digests, digest)
		return nil
	}); err != nil {
		return nil, err
	}

	return digests, nil
}

// ListReferences returns the set of reference names stored in the image.
func (e dirEngine) ListReferences(ctx context.Context) ([]string, error) {
	refs := []string{}
	refDir := filepath.Join(e.path, refDirectory)

	if err := filepath.Walk(refDir, func(path string, _ os.FileInfo, _ error) error {
		// Skip the actual directory.
		if path == refDir {
			return nil
		}

		// XXX: Do we need to handle multiple-directory-deep cases?
		refs = append(refs, filepath.Base(path))
		return nil
	}); err != nil {
		return nil, err
	}

	return refs, nil
}

// Close releases all references held by the e. Subsequent operations may
// fail.
func (e dirEngine) Close() error {
	return os.RemoveAll(e.temp)
}

func newDirEngine(path string) (*dirEngine, error) {
	engine := &dirEngine{
		path: path,
	}

	if err := engine.validate(); err != nil {
		return nil, err
	}

	tempDir, err := ioutil.TempDir(engine.path, "tmp-")
	if err != nil {
		return nil, err
	}
	engine.temp = tempDir
	return engine, nil
}
