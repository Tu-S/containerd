// +build windows

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package windows

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"time"

	winio "github.com/Microsoft/go-winio"
	"github.com/containerd/containerd/archive"
	"github.com/containerd/containerd/archive/compression"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/diff"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/metadata"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/containerd/plugin"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

func init() {
	plugin.Register(&plugin.Registration{
		Type: plugin.DiffPlugin,
		ID:   "windows",
		Requires: []plugin.Type{
			plugin.MetadataPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			md, err := ic.Get(plugin.MetadataPlugin)
			if err != nil {
				return nil, err
			}

			ic.Meta.Platforms = append(ic.Meta.Platforms, platforms.DefaultSpec())
			return NewWindowsDiff(md.(*metadata.DB).ContentStore())
		},
	})
}

// CompareApplier handles both comparison and
// application of layer diffs.
type CompareApplier interface {
	diff.Applier
	diff.Comparer
}

// windowsDiff does filesystem comparison and application
// for Windows specific layer diffs.
type windowsDiff struct {
	store content.Store
}

var emptyDesc = ocispec.Descriptor{}
var uncompressed = "containerd.io/uncompressed"

// NewWindowsDiff is the Windows container layer implementation
// for comparing and applying filesystem layers
func NewWindowsDiff(store content.Store) (CompareApplier, error) {
	return windowsDiff{
		store: store,
	}, nil
}

// Apply applies the content associated with the provided digests onto the
// provided mounts. Archive content will be extracted and decompressed if
// necessary.
func (s windowsDiff) Apply(ctx context.Context, desc ocispec.Descriptor, mounts []mount.Mount, opts ...diff.ApplyOpt) (d ocispec.Descriptor, err error) {
	t1 := time.Now()
	defer func() {
		if err == nil {
			log.G(ctx).WithFields(logrus.Fields{
				"d":      time.Since(t1),
				"digest": desc.Digest,
				"size":   desc.Size,
				"media":  desc.MediaType,
			}).Debug("diff applied")
		}
	}()

	var config diff.ApplyConfig
	for _, o := range opts {
		if err := o(ctx, desc, &config); err != nil {
			return emptyDesc, errors.Wrap(err, "failed to apply config opt")
		}
	}

	ra, err := s.store.ReaderAt(ctx, desc)
	if err != nil {
		return emptyDesc, errors.Wrap(err, "failed to get reader from content store")
	}
	defer ra.Close()

	processor := diff.NewProcessorChain(desc.MediaType, content.NewReader(ra))
	for {
		if processor, err = diff.GetProcessor(ctx, processor, config.ProcessorPayloads); err != nil {
			return emptyDesc, errors.Wrapf(err, "failed to get stream processor for %s", desc.MediaType)
		}
		if processor.MediaType() == ocispec.MediaTypeImageLayer {
			break
		}
	}
	defer processor.Close()

	digester := digest.Canonical.Digester()
	rc := &readCounter{
		r: io.TeeReader(processor, digester.Hash()),
	}

	layer, parentLayerPaths, err := mountsToLayerAndParents(mounts)
	if err != nil {
		return emptyDesc, err
	}

	// TODO darrenstahlmsft: When this is done isolated, we should disable these.
	// it currently cannot be disabled, unless we add ref counting. Since this is
	// temporary, leaving it enabled is OK for now.
	// https://github.com/containerd/containerd/issues/1681
	if err := winio.EnableProcessPrivileges([]string{winio.SeBackupPrivilege, winio.SeRestorePrivilege}); err != nil {
		return emptyDesc, err
	}

	if _, err := archive.Apply(ctx, layer, rc, archive.WithParents(parentLayerPaths), archive.AsWindowsContainerLayer()); err != nil {
		return emptyDesc, err
	}

	// Read any trailing data
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return emptyDesc, err
	}

	return ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayer,
		Size:      rc.c,
		Digest:    digester.Digest(),
	}, nil
}

// Compare creates a diff between the given mounts and uploads the result
// to the content store.
func (s windowsDiff) Compare(ctx context.Context, lower, upper []mount.Mount, opts ...diff.Opt) (d ocispec.Descriptor, err error) {
	t1 := time.Now()

	var config diff.Config
	for _, opt := range opts {
		if err := opt(&config); err != nil {
			return emptyDesc, err
		}
	}

	layers, err := mountPairToLayerStack(lower, upper)
	if err != nil {
		return emptyDesc, err
	}

	if config.MediaType == "" {
		config.MediaType = ocispec.MediaTypeImageLayerGzip
	}

	var isCompressed bool
	switch config.MediaType {
	case ocispec.MediaTypeImageLayer:
	case ocispec.MediaTypeImageLayerGzip:
		isCompressed = true
	default:
		return emptyDesc, errors.Wrapf(errdefs.ErrNotImplemented, "unsupported diff media type: %v", config.MediaType)
	}

	newReference := false
	if config.Reference == "" {
		newReference = true
		config.Reference = uniqueRef()
	}

	cw, err := s.store.Writer(ctx, content.WithRef(config.Reference), content.WithDescriptor(ocispec.Descriptor{
		MediaType: config.MediaType,
	}))

	if err != nil {
		return emptyDesc, errors.Wrap(err, "failed to open writer")
	}

	defer func() {
		if err != nil {
			cw.Close()
			if newReference {
				if abortErr := s.store.Abort(ctx, config.Reference); abortErr != nil {
					log.G(ctx).WithError(abortErr).WithField("ref", config.Reference).Warnf("failed to delete diff upload")
				}
			}
		}
	}()

	if !newReference {
		if err = cw.Truncate(0); err != nil {
			return emptyDesc, err
		}
	}

	// TODO darrenstahlmsft: When this is done isolated, we should disable this.
	// it currently cannot be disabled, unless we add ref counting. Since this is
	// temporary, leaving it enabled is OK for now.
	// https://github.com/containerd/containerd/issues/1681
	if err := winio.EnableProcessPrivileges([]string{winio.SeBackupPrivilege}); err != nil {
		return emptyDesc, err
	}

	if isCompressed {
		dgstr := digest.SHA256.Digester()
		var compressed io.WriteCloser
		compressed, err = compression.CompressStream(cw, compression.Gzip)
		if err != nil {
			return emptyDesc, errors.Wrap(err, "failed to get compressed stream")
		}
		err = archive.WriteDiff(ctx, io.MultiWriter(compressed, dgstr.Hash()), "", layers[0], archive.AsWindowsContainerLayerPair(), archive.WithParentLayers(layers[1:]))
		compressed.Close()
		if err != nil {
			return emptyDesc, errors.Wrap(err, "failed to write compressed diff")
		}

		if config.Labels == nil {
			config.Labels = map[string]string{}
		}
		config.Labels[uncompressed] = dgstr.Digest().String()
	} else {
		if err = archive.WriteDiff(ctx, cw, "", layers[0], archive.AsWindowsContainerLayerPair(), archive.WithParentLayers(layers[1:])); err != nil {
			return emptyDesc, errors.Wrap(err, "failed to write diff")
		}
	}

	var commitopts []content.Opt
	if config.Labels != nil {
		commitopts = append(commitopts, content.WithLabels(config.Labels))
	}

	dgst := cw.Digest()
	if err := cw.Commit(ctx, 0, dgst, commitopts...); err != nil {
		if !errdefs.IsAlreadyExists(err) {
			return emptyDesc, errors.Wrap(err, "failed to commit")
		}
	}

	info, err := s.store.Info(ctx, dgst)
	if err != nil {
		return emptyDesc, errors.Wrap(err, "failed to get info from content store")
	}
	if info.Labels == nil {
		info.Labels = make(map[string]string)
	}
	// Set uncompressed label if digest already existed without label
	if _, ok := info.Labels[uncompressed]; !ok {
		info.Labels[uncompressed] = config.Labels[uncompressed]
		if _, err := s.store.Update(ctx, info, "labels."+uncompressed); err != nil {
			return emptyDesc, errors.Wrap(err, "error setting uncompressed label")
		}
	}

	desc := ocispec.Descriptor{
		MediaType: config.MediaType,
		Size:      info.Size,
		Digest:    info.Digest,
	}

	log.G(ctx).WithFields(logrus.Fields{
		"d":     time.Since(t1),
		"dgst":  desc.Digest,
		"size":  desc.Size,
		"media": desc.MediaType,
	}).Debug("diff created")

	return desc, nil
}

type readCounter struct {
	r io.Reader
	c int64
}

func (rc *readCounter) Read(p []byte) (n int, err error) {
	n, err = rc.r.Read(p)
	rc.c += int64(n)
	return
}

func mountsToLayerAndParents(mounts []mount.Mount) (string, []string, error) {
	if len(mounts) != 1 {
		return "", nil, errors.Wrap(errdefs.ErrInvalidArgument, "number of mounts should always be 1 for Windows layers")
	}
	mnt := mounts[0]
	if mnt.Type != "windows-layer" {
		// This is a special case error. When this is received the diff service
		// will attempt the next differ in the chain which for Windows is the
		// lcow differ that we want.
		return "", nil, errors.Wrapf(errdefs.ErrNotImplemented, "windowsDiff does not support layer type %s", mnt.Type)
	}

	parentLayerPaths, err := mnt.GetParentPaths()
	if err != nil {
		return "", nil, err
	}

	return mnt.Source, parentLayerPaths, nil
}

// mountPairToLayerStack ensures that the two sets of mount-lists are actually a correct
// parent-and-child, or orphan-and-empty-list, and return the full list of layers, starting
// with the upper-most (most childish?)
func mountPairToLayerStack(lower, upper []mount.Mount) ([]string, error) {

	// May return an ErrNotImplemented, which will fall back to LCOW
	upperLayer, upperParentLayerPaths, err := mountsToLayerAndParents(upper)
	if err != nil {
		return nil, errors.Wrapf(err, "Upper mount invalid")
	}

	// Trivial case, diff-against-nothing
	if len(lower) == 0 {
		if len(upperParentLayerPaths) != 0 {
			return nil, errors.Wrap(errdefs.ErrInvalidArgument, "windowsDiff cannot diff a layer with parents against a null layer")
		}
		return []string{upperLayer}, nil
	}

	if len(upperParentLayerPaths) < 1 {
		return nil, errors.Wrap(errdefs.ErrInvalidArgument, "windowsDiff cannot diff a layer with no parents against another layer")
	}

	lowerLayer, lowerParentLayerPaths, err := mountsToLayerAndParents(lower)
	if errdefs.IsNotImplemented(err) {
		// Upper was a windows-layer, lower is not. We can't handle that.
		return nil, errors.Wrap(errdefs.ErrInvalidArgument, "windowsDiff cannot diff a windows-layer against a non-windows-layer")
	} else if err != nil {
		return nil, errors.Wrapf(err, "Lower mount invalid")
	}

	if upperParentLayerPaths[0] != lowerLayer {
		return nil, errors.Wrap(errdefs.ErrInvalidArgument, "windowsDiff cannot diff a layer against a layer other than its own parent")
	}

	if len(upperParentLayerPaths) != len(lowerParentLayerPaths)+1 {
		return nil, errors.Wrap(errdefs.ErrInvalidArgument, "windowsDiff cannot diff a layer against a layer with different parents")
	}
	for i, upperParent := range upperParentLayerPaths[1:] {
		if upperParent != lowerParentLayerPaths[i] {
			return nil, errors.Wrap(errdefs.ErrInvalidArgument, "windowsDiff cannot diff a layer against a layer with different parents")
		}
	}

	return append([]string{upperLayer}, upperParentLayerPaths...), nil
}

func uniqueRef() string {
	t := time.Now()
	var b [3]byte
	// Ignore read failures, just decreases uniqueness
	rand.Read(b[:])
	return fmt.Sprintf("%d-%s", t.UnixNano(), base64.URLEncoding.EncodeToString(b[:]))
}
