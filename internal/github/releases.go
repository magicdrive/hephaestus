package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// Release is a GitHub release.
type Release struct {
	ID              int64   `json:"id"`
	TagName         string  `json:"tag_name"`
	TargetCommitish string  `json:"target_commitish"`
	Name            string  `json:"name"`
	Body            string  `json:"body"`
	Draft           bool    `json:"draft"`
	Prerelease      bool    `json:"prerelease"`
	Immutable       bool    `json:"immutable"`
	HTMLURL         string  `json:"html_url"`
	UploadURL       string  `json:"upload_url"`
	Assets          []Asset `json:"assets"`
}

// Asset states. Only AssetStateUploaded is complete; GitHub also reports
// "starter" for an asset left behind by a failed upload.
const (
	AssetStateUploaded = "uploaded"
	AssetStateOpen     = "open"
	AssetStateStarter  = "starter"
)

// Asset is a release asset.
type Asset struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Label       string `json:"label"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	State       string `json:"state"`
	// Digest is "sha256:<hex>"; nil for assets uploaded before GitHub
	// started computing digests (June 2025).
	Digest             *string `json:"digest"`
	BrowserDownloadURL string  `json:"browser_download_url"`
	URL                string  `json:"url"`
}

// ReleaseRequest is the body of create and update calls. Nil pointers and
// empty strings are omitted, so an update changes only what is set.
type ReleaseRequest struct {
	TagName string `json:"tag_name,omitempty"`
	// TargetCommitish is used by GitHub only when the tag does not exist:
	// the tag is then created from it when the release is published.
	TargetCommitish string  `json:"target_commitish,omitempty"`
	Name            *string `json:"name,omitempty"`
	Body            *string `json:"body,omitempty"`
	Draft           *bool   `json:"draft,omitempty"`
	Prerelease      *bool   `json:"prerelease,omitempty"`
	// MakeLatest is "true", "false" or "legacy". Drafts and prereleases
	// cannot be latest.
	MakeLatest string `json:"make_latest,omitempty"`
}

// GetReleaseByTag returns the published release for tag. Drafts are not
// returned by this endpoint; use FindReleaseByTag to include them.
func (c *Client) GetReleaseByTag(ctx context.Context, owner, repo, tag string) (*Release, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	if tag == "" {
		return nil, errors.New("github: empty tag")
	}
	var r Release
	if err := c.doJSON(ctx, http.MethodGet, repoURL(c.api, owner, repo, nil, "releases", "tags", tag), nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// GetRelease returns a release by id.
func (c *Client) GetRelease(ctx context.Context, owner, repo string, id int64) (*Release, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	var r Release
	if err := c.doJSON(ctx, http.MethodGet, repoURL(c.api, owner, repo, nil, "releases", itoa(id)), nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ListReleases returns every release, including drafts when the token has
// push access.
func (c *Client) ListReleases(ctx context.Context, owner, repo string) ([]Release, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	return listAll[Release](ctx, c, repoURL(c.api, owner, repo, pageQuery(), "releases"))
}

// FindReleaseByTag searches the release list (drafts included) for tag. A
// published release wins over drafts; among several drafts for the same tag
// the oldest (lowest id) is returned. It returns an error matching
// ErrNotFound when there is none.
func (c *Client) FindReleaseByTag(ctx context.Context, owner, repo, tag string) (*Release, error) {
	all, err := c.ListReleases(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	var best *Release
	for i := range all {
		r := &all[i]
		if r.TagName != tag {
			continue
		}
		switch {
		case best == nil,
			best.Draft && !r.Draft,
			best.Draft == r.Draft && r.ID < best.ID:
			best = r
		}
	}
	if best == nil {
		return nil, fmt.Errorf("release for tag %q: %w", tag, ErrNotFound)
	}
	return best, nil
}

// CreateRelease creates a release. It is never retried automatically: on a
// transient failure the release may exist, so re-read before retrying.
func (c *Client) CreateRelease(ctx context.Context, owner, repo string, req ReleaseRequest) (*Release, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	if req.TagName == "" {
		return nil, errors.New("github: create release: tag_name is required")
	}
	var r Release
	if err := c.doJSON(ctx, http.MethodPost, repoURL(c.api, owner, repo, nil, "releases"), req, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// UpdateRelease changes the fields set in req.
func (c *Client) UpdateRelease(ctx context.Context, owner, repo string, id int64, req ReleaseRequest) (*Release, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	var r Release
	if err := c.doJSON(ctx, http.MethodPatch, repoURL(c.api, owner, repo, nil, "releases", itoa(id)), req, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ListReleaseAssets returns every asset of a release.
func (c *Client) ListReleaseAssets(ctx context.Context, owner, repo string, releaseID int64) ([]Asset, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	return listAll[Asset](ctx, c, repoURL(c.api, owner, repo, pageQuery(), "releases", itoa(releaseID), "assets"))
}

// UploadReleaseAsset uploads exactly size bytes from r as asset name. It is
// never retried automatically: after a failure, list the assets, delete any
// incomplete one ("starter") and upload again.
func (c *Client) UploadReleaseAsset(ctx context.Context, owner, repo string, releaseID int64, name string, r io.Reader, size int64, contentType string) (*Asset, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	if name == "" {
		return nil, errors.New("github: upload: empty asset name")
	}
	if size < 0 {
		return nil, errors.New("github: upload: negative size")
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	var a Asset
	_, err := c.do(ctx, &call{
		method:      http.MethodPost,
		url:         repoURL(c.upload, owner, repo, url.Values{"name": {name}}, "releases", itoa(releaseID), "assets"),
		stream:      r,
		size:        size,
		contentType: contentType,
		timeout:     c.xferTimeout,
		handle:      readJSON(&a),
	})
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// DeleteReleaseAsset deletes an asset. A 404 is returned as an error
// matching ErrNotFound.
func (c *Client) DeleteReleaseAsset(ctx context.Context, owner, repo string, assetID int64) error {
	if err := checkRepo(owner, repo); err != nil {
		return err
	}
	return c.doJSON(ctx, http.MethodDelete, repoURL(c.api, owner, repo, nil, "releases", "assets", itoa(assetID)), nil, nil)
}

// DownloadReleaseAsset writes the asset's content to w and returns the
// number of bytes written. More than maxBytes is an error. The redirect to
// object storage is followed without the Authorization header.
func (c *Client) DownloadReleaseAsset(ctx context.Context, owner, repo string, assetID int64, w io.Writer, maxBytes int64) (int64, error) {
	if err := checkRepo(owner, repo); err != nil {
		return 0, err
	}
	var n int64
	_, err := c.do(ctx, &call{
		method:  http.MethodGet,
		url:     repoURL(c.api, owner, repo, nil, "releases", "assets", itoa(assetID)),
		accept:  "application/octet-stream",
		timeout: c.xferTimeout,
		handle: func(resp *http.Response) error {
			n = 0
			m, err := io.Copy(w, io.LimitReader(resp.Body, maxBytes+1))
			n = m
			if err != nil {
				return fmt.Errorf("github: downloading asset %d: %w", assetID, err)
			}
			if m > maxBytes {
				return fmt.Errorf("github: asset %d exceeds %d bytes", assetID, maxBytes)
			}
			return nil
		},
	})
	return n, err
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
