package terminalpet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "golang.org/x/image/webp"

	"github.com/Gitlawb/zero/internal/installtxn"
)

const (
	manifestCacheAge = 10 * time.Minute
	rankingCacheAge  = 10 * time.Minute
	maxManifestBytes = 2 << 20
	maxRankingBytes  = 2 << 20
	maxPreviewBytes  = 2 << 20
	maxSpriteBytes   = 16 << 20
	maxPetJSONBytes  = 1 << 20
	maxImageSide     = 16384
	maxImagePixels   = 50_000_000
)

type Client struct {
	RootDir      string
	ManifestURL  string
	RankingURL   string
	HTTPClient   *http.Client
	TrustedHosts map[string]bool
	// TrustedAssetHosts is narrower than TrustedHosts: catalog redirects may use
	// petdex.dev, while image and metadata downloads must stay on the asset host.
	TrustedAssetHosts map[string]bool
	Now               func() time.Time
}

type compactManifest struct {
	Version   int               `json:"v"`
	AssetBase string            `json:"assetBase"`
	Fields    []string          `json:"fields"`
	Pets      []json.RawMessage `json:"pets"`
}

type installedMetadata struct {
	Entry
	InstalledAt time.Time `json:"installedAt"`
}

type petMetadata struct {
	SpriteVersion int                             `json:"spriteVersionNumber"`
	Animations    map[string]petAnimationMetadata `json:"animations"`
	Interactions  struct {
		Click struct {
			Animations []string `json:"animations"`
		} `json:"click"`
	} `json:"interactions"`
}

type petAnimationMetadata struct {
	SourceRow      json.RawMessage `json:"sourceRow"`
	SourceRowIndex *int            `json:"sourceRowIndex"`
	FrameCount     int             `json:"frameCount"`
	TimingMS       []int           `json:"timingMs"`
	Playback       string          `json:"playback"`
	Loop           bool            `json:"loop"`
}

func NewClient(rootDir string) *Client {
	client := &Client{
		RootDir:     rootDir,
		ManifestURL: DefaultManifestURL,
		RankingURL:  DefaultRankingURL,
		TrustedHosts: map[string]bool{
			"petdex.dev":     true,
			TrustedAssetHost: true,
		},
		TrustedAssetHosts: map[string]bool{TrustedAssetHost: true},
		Now:               time.Now,
	}
	client.HTTPClient = &http.Client{Timeout: 20 * time.Second}
	return client
}

func (c *Client) Catalog(ctx context.Context) ([]Entry, error) {
	remote, remoteErr := c.remoteCatalog(ctx)
	local, localErr := c.localCatalog()
	entries := mergeEntries(local, remote)
	if len(entries) > 0 {
		rankingCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		ranking, _ := c.installedRanking(rankingCtx)
		cancel()
		sortEntriesByRanking(entries, ranking)
		return entries, nil
	}
	return nil, errors.Join(remoteErr, localErr)
}

func (c *Client) InstalledEntries() ([]Entry, error) {
	return c.localCatalog()
}

type rankingPayload struct {
	Pets []struct {
		Slug string `json:"slug"`
	} `json:"pets"`
}

func (c *Client) installedRanking(ctx context.Context) ([]string, error) {
	cachePath := filepath.Join(c.cacheDir(), "ranking-installed.json")
	if info, err := os.Stat(cachePath); err == nil && c.now().Sub(info.ModTime()) < rankingCacheAge {
		if data, readErr := os.ReadFile(cachePath); readErr == nil {
			if ranking, decodeErr := decodeRanking(data); decodeErr == nil {
				return ranking, nil
			}
		}
	}
	data, err := c.fetch(ctx, c.RankingURL, maxRankingBytes)
	if err == nil {
		ranking, decodeErr := decodeRanking(data)
		if decodeErr == nil {
			_ = c.cacheFile(cachePath, data)
			return ranking, nil
		}
		err = decodeErr
	}
	if cached, readErr := os.ReadFile(cachePath); readErr == nil {
		if ranking, decodeErr := decodeRanking(cached); decodeErr == nil {
			return ranking, nil
		}
	}
	return nil, fmt.Errorf("load pet ranking: %w", err)
}

func decodeRanking(data []byte) ([]string, error) {
	var payload rankingPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode pet ranking: %w", err)
	}
	ranking := make([]string, 0, len(payload.Pets))
	seen := make(map[string]bool, len(payload.Pets))
	for _, pet := range payload.Pets {
		slug := strings.TrimSpace(pet.Slug)
		if validateSlug(slug) != nil || seen[slug] {
			continue
		}
		seen[slug] = true
		ranking = append(ranking, slug)
	}
	if len(ranking) == 0 {
		return nil, fmt.Errorf("pet ranking is empty")
	}
	return ranking, nil
}

func (c *Client) Preview(ctx context.Context, entry Entry) (*Animation, error) {
	if entry.Local {
		if animation, err := c.LoadInstalled(entry.Slug); err == nil {
			return animation, nil
		}
	}
	if err := validateSlug(entry.Slug); err != nil {
		return nil, err
	}
	cachePath := filepath.Join(c.previewDir(), previewCacheName(entry))
	if data, err := os.ReadFile(cachePath); err == nil {
		if animation, err := decodePreview(data); err == nil {
			return animation, nil
		}
	}
	assetBase, err := c.trustedAssetBase(entry.AssetBase)
	if err != nil {
		return nil, err
	}
	previewURL := assetBase + "/pets/" + url.PathEscape(entry.Slug) + "/preview.webp"
	data, err := c.fetchAsset(ctx, previewURL, maxPreviewBytes)
	if err == nil {
		animation, decodeErr := decodePreview(data)
		if decodeErr == nil {
			_ = c.cacheFile(cachePath, data)
			return animation, nil
		}
		err = decodeErr
	}
	thumbnailURL := assetBase + "/pets/" + url.PathEscape(entry.Slug) + "/thumb.webp"
	thumbnail, thumbnailErr := c.fetchAsset(ctx, thumbnailURL, maxPreviewBytes)
	if thumbnailErr != nil {
		return nil, errors.Join(err, thumbnailErr)
	}
	imageValue, decodeErr := decodeImage(thumbnail)
	if decodeErr != nil {
		return nil, decodeErr
	}
	return ThumbnailAnimation(imageValue)
}

func (c *Client) Install(ctx context.Context, entry Entry) (*Animation, error) {
	if err := validateSlug(entry.Slug); err != nil {
		return nil, err
	}
	if entry.Local {
		return c.LoadInstalled(entry.Slug)
	}
	spriteURL, err := c.trustedAssetURL(entry.SpritesheetURL)
	if err != nil {
		return nil, fmt.Errorf("invalid spritesheet URL: %w", err)
	}
	spriteData, err := c.fetchAsset(ctx, spriteURL, maxSpriteBytes)
	if err != nil {
		return nil, fmt.Errorf("download spritesheet: %w", err)
	}
	sheet, err := decodeImage(spriteData)
	if err != nil {
		return nil, fmt.Errorf("decode spritesheet: %w", err)
	}
	var petJSON []byte
	var animationTracks map[State]atlasTrack
	var document petMetadata
	if strings.TrimSpace(entry.PetJSONURL) != "" {
		petURL, urlErr := c.trustedAssetURL(entry.PetJSONURL)
		if urlErr != nil {
			return nil, fmt.Errorf("invalid pet metadata URL: %w", urlErr)
		}
		petJSON, err = c.fetchAsset(ctx, petURL, maxPetJSONBytes)
		if err != nil {
			return nil, fmt.Errorf("download pet metadata: %w", err)
		}
		if err := json.Unmarshal(petJSON, &document); err != nil {
			return nil, fmt.Errorf("invalid pet metadata: %w", err)
		}
		if document.SpriteVersion != 0 {
			if document.SpriteVersion != 1 && document.SpriteVersion != 2 {
				return nil, fmt.Errorf("invalid pet metadata: unsupported sprite version %d", document.SpriteVersion)
			}
			entry.SpriteVersion = document.SpriteVersion
		}
		animationTracks, err = document.atlasTracks()
		if err != nil {
			return nil, fmt.Errorf("invalid pet metadata: %w", err)
		}
	}
	animation, err := atlasAnimation(sheet, entry.SpriteVersion, animationTracks)
	if err != nil {
		return nil, err
	}
	animation.setClickAnimations(document.Interactions.Click.Animations)

	root := c.installedDir()
	stage, cleanup, err := installtxn.StageDir(root)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return nil, fmt.Errorf("create pet staging directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(stage, "spritesheet.webp"), spriteData, 0o600); err != nil {
		return nil, fmt.Errorf("stage spritesheet: %w", err)
	}
	if len(petJSON) > 0 {
		if err := os.WriteFile(filepath.Join(stage, "pet.json"), petJSON, 0o600); err != nil {
			return nil, fmt.Errorf("stage pet metadata: %w", err)
		}
	}
	metadata, err := json.MarshalIndent(installedMetadata{Entry: entry, InstalledAt: c.now()}, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(stage, "source.json"), append(metadata, '\n'), 0o600); err != nil {
		return nil, fmt.Errorf("stage pet source: %w", err)
	}
	unlock, err := installtxn.Lock(root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	// Put back anything an earlier run was killed mid-commit; see installtxn.Recover,
	// and abort on a transaction it could not resolve rather than install over a
	// tree that is still owed a restore. The reconciler is nil because this commit
	// publishes no metadata beside the tree (the publish is a no-op), so there is
	// nothing to reconcile a retained backup against and a live target is the
	// committed one.
	if err := installtxn.Recover(root, nil); err != nil {
		return nil, err
	}
	target := filepath.Join(root, entry.Slug)
	if err := installtxn.CommitDir(target, stage, func() error { return nil }); err != nil {
		return nil, fmt.Errorf("install pet: %w", err)
	}
	return animation, nil
}

func (c *Client) LoadInstalled(slug string) (*Animation, error) {
	if err := validateSlug(slug); err != nil {
		return nil, err
	}
	dir := filepath.Join(c.installedDir(), slug)
	entry, err := c.InstalledEntry(slug)
	if err != nil {
		return nil, err
	}
	spriteData, err := os.ReadFile(filepath.Join(dir, "spritesheet.webp"))
	if err != nil {
		return nil, fmt.Errorf("read installed spritesheet: %w", err)
	}
	sheet, err := decodeImage(spriteData)
	if err != nil {
		return nil, err
	}
	var animationTracks map[State]atlasTrack
	var document petMetadata
	if petJSON, readErr := os.ReadFile(filepath.Join(dir, "pet.json")); readErr == nil {
		if decodeErr := json.Unmarshal(petJSON, &document); decodeErr != nil {
			return nil, fmt.Errorf("read installed pet metadata: %w", decodeErr)
		}
		if document.SpriteVersion != 0 {
			entry.SpriteVersion = document.SpriteVersion
		}
		animationTracks, err = document.atlasTracks()
		if err != nil {
			return nil, fmt.Errorf("read installed pet metadata: %w", err)
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return nil, fmt.Errorf("read installed pet metadata: %w", readErr)
	}
	animation, err := atlasAnimation(sheet, entry.SpriteVersion, animationTracks)
	if err != nil {
		return nil, err
	}
	animation.setClickAnimations(document.Interactions.Click.Animations)
	return animation, nil
}

func (m petMetadata) atlasTracks() (map[State]atlasTrack, error) {
	names := make([]string, 0, len(m.Animations))
	for name := range m.Animations {
		names = append(names, name)
	}
	sort.Strings(names)
	tracks := make(map[State]atlasTrack)
	for _, name := range names {
		spec := m.Animations[name]
		state := State(strings.ToLower(strings.TrimSpace(name)))
		if state == "" {
			return nil, fmt.Errorf("animation name is empty")
		}
		if spec.FrameCount < 1 || spec.FrameCount > atlasColumns {
			return nil, fmt.Errorf("animation %q has invalid frame count %d", name, spec.FrameCount)
		}
		if len(spec.TimingMS) != 0 && len(spec.TimingMS) != spec.FrameCount {
			return nil, fmt.Errorf("animation %q has %d timings for %d frames", name, len(spec.TimingMS), spec.FrameCount)
		}
		row, err := spec.rowIndex()
		if err != nil {
			return nil, fmt.Errorf("animation %q: %w", name, err)
		}
		// Idle must always cycle. Treating an authored idle track as one-shot
		// leaves the companion frozen on its final frame indefinitely.
		loop := state == Idle || spec.Loop || strings.EqualFold(strings.TrimSpace(spec.Playback), "loop")
		track := atlasTrack{row: row, count: spec.FrameCount, loop: loop, fallbackIdle: !loop}
		if len(spec.TimingMS) > 0 {
			track.durations = make([]time.Duration, len(spec.TimingMS))
			for index, milliseconds := range spec.TimingMS {
				if milliseconds < 16 || milliseconds > 10_000 {
					return nil, fmt.Errorf("animation %q has invalid frame timing %dms", name, milliseconds)
				}
				track.durations[index] = time.Duration(milliseconds) * time.Millisecond
			}
		}
		tracks[state] = track
	}
	return tracks, nil
}

func (m petAnimationMetadata) rowIndex() (int, error) {
	if m.SourceRowIndex != nil {
		return *m.SourceRowIndex, nil
	}
	if len(m.SourceRow) == 0 || string(m.SourceRow) == "null" {
		return 0, fmt.Errorf("source row is missing")
	}
	var numeric int
	if err := json.Unmarshal(m.SourceRow, &numeric); err == nil {
		return numeric, nil
	}
	var name string
	if err := json.Unmarshal(m.SourceRow, &name); err != nil {
		return 0, fmt.Errorf("source row is invalid")
	}
	rows := map[string]int{
		"idle": 0, "running-right": 1, "running-left": 2, "waving": 3,
		"jumping": 4, "failed": 5, "waiting": 6, "running": 7, "review": 8,
	}
	normalized := strings.ToLower(strings.TrimSpace(name))
	if row, ok := rows[normalized]; ok {
		return row, nil
	}
	if row, err := strconv.Atoi(normalized); err == nil {
		return row, nil
	}
	return 0, fmt.Errorf("unknown source row %q", name)
}

func (c *Client) InstalledEntry(slug string) (Entry, error) {
	if err := validateSlug(slug); err != nil {
		return Entry{}, err
	}
	metadataData, err := os.ReadFile(filepath.Join(c.installedDir(), slug, "source.json"))
	if err != nil {
		return Entry{}, fmt.Errorf("read installed pet: %w", err)
	}
	var metadata installedMetadata
	if err := json.Unmarshal(metadataData, &metadata); err != nil {
		return Entry{}, fmt.Errorf("read installed pet metadata: %w", err)
	}
	if metadata.Slug != slug {
		return Entry{}, fmt.Errorf("installed pet metadata does not match %q", slug)
	}
	metadata.Local = true
	return metadata.Entry, nil
}

func (c *Client) remoteCatalog(ctx context.Context) ([]Entry, error) {
	cachePath := filepath.Join(c.cacheDir(), "petdex-v2.json")
	if info, err := os.Stat(cachePath); err == nil && c.now().Sub(info.ModTime()) < manifestCacheAge {
		if data, readErr := os.ReadFile(cachePath); readErr == nil {
			if entries, decodeErr := c.decodeCatalog(data); decodeErr == nil {
				return entries, nil
			}
		}
	}
	data, err := c.fetch(ctx, c.ManifestURL, maxManifestBytes)
	if err == nil {
		entries, decodeErr := c.decodeCatalog(data)
		if decodeErr == nil {
			_ = c.cacheFile(cachePath, data)
			return entries, nil
		}
		err = decodeErr
	}
	if cached, readErr := os.ReadFile(cachePath); readErr == nil {
		if entries, decodeErr := c.decodeCatalog(cached); decodeErr == nil {
			return entries, nil
		}
	}
	return nil, fmt.Errorf("load pet catalog: %w", err)
}

func (c *Client) decodeCatalog(data []byte) ([]Entry, error) {
	var manifest compactManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("decode pet catalog: %w", err)
	}
	if manifest.Version != 2 {
		return nil, fmt.Errorf("unsupported pet catalog version %d", manifest.Version)
	}
	assetBase, err := c.trustedAssetBase(manifest.AssetBase)
	if err != nil {
		return nil, err
	}
	wanted := map[string]int{}
	for index, field := range manifest.Fields {
		wanted[field] = index
	}
	required := []string{"slug", "displayName", "kind", "submittedBy", "spritesheet", "petJson", "spriteVersionNumber"}
	for _, field := range required {
		if _, ok := wanted[field]; !ok {
			return nil, fmt.Errorf("pet catalog is missing %q", field)
		}
	}
	entries := make([]Entry, 0, len(manifest.Pets))
	seen := map[string]bool{}
	var firstRowError error
	for _, rawRow := range manifest.Pets {
		entry, rowErr := func() (Entry, error) {
			var row []json.RawMessage
			if err := json.Unmarshal(rawRow, &row); err != nil {
				return Entry{}, fmt.Errorf("decode pet catalog row: %w", err)
			}
			value := func(field string, target any) error {
				index := wanted[field]
				if index >= len(row) {
					return fmt.Errorf("pet catalog row is missing %q", field)
				}
				return json.Unmarshal(row[index], target)
			}
			var entry Entry
			if err := value("slug", &entry.Slug); err != nil {
				return Entry{}, err
			}
			if err := validateSlug(entry.Slug); err != nil {
				return Entry{}, err
			}
			if err := value("displayName", &entry.DisplayName); err != nil {
				return Entry{}, err
			}
			if err := value("kind", &entry.Kind); err != nil {
				return Entry{}, err
			}
			// submittedBy is nullable in the public manifest; null intentionally maps
			// to an empty creator label.
			_ = value("submittedBy", &entry.SubmittedBy)
			if err := value("spritesheet", &entry.SpritesheetURL); err != nil {
				return Entry{}, err
			}
			if err := value("petJson", &entry.PetJSONURL); err != nil {
				return Entry{}, err
			}
			if err := value("spriteVersionNumber", &entry.SpriteVersion); err != nil {
				return Entry{}, err
			}
			if entry.SpriteVersion != 1 && entry.SpriteVersion != 2 {
				return Entry{}, fmt.Errorf("pet %q has unsupported sprite version %d", entry.Slug, entry.SpriteVersion)
			}
			entry.AssetBase = assetBase
			resolved, err := c.resolveAssetURL(assetBase, entry.SpritesheetURL)
			if err != nil {
				return Entry{}, fmt.Errorf("pet %q: %w", entry.Slug, err)
			}
			entry.SpritesheetURL = resolved
			if strings.TrimSpace(entry.PetJSONURL) != "" {
				resolved, err = c.resolveAssetURL(assetBase, entry.PetJSONURL)
				if err != nil {
					return Entry{}, fmt.Errorf("pet %q: %w", entry.Slug, err)
				}
				entry.PetJSONURL = resolved
			}
			return entry, nil
		}()
		if rowErr != nil {
			if firstRowError == nil {
				firstRowError = rowErr
			}
			continue
		}
		if seen[entry.Slug] {
			continue
		}
		seen[entry.Slug] = true
		entries = append(entries, entry)
	}
	if len(entries) == 0 && firstRowError != nil {
		return nil, firstRowError
	}
	sort.Slice(entries, func(i, j int) bool { return strings.ToLower(entries[i].Label()) < strings.ToLower(entries[j].Label()) })
	return entries, nil
}

func (c *Client) localCatalog() ([]Entry, error) {
	dirs, err := os.ReadDir(c.installedDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(dirs))
	for _, dir := range dirs {
		if !dir.IsDir() || validateSlug(dir.Name()) != nil {
			continue
		}
		entry, readErr := c.InstalledEntry(dir.Name())
		if readErr != nil {
			continue
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func (c *Client) fetch(ctx context.Context, address string, limit int64) ([]byte, error) {
	return c.fetchLimit(ctx, address, limit, false)
}

func (c *Client) fetchAsset(ctx context.Context, address string, limit int64) ([]byte, error) {
	return c.fetchLimit(ctx, address, limit, true)
}

func (c *Client) fetchLimit(ctx context.Context, address string, limit int64, assetOnly bool) ([]byte, error) {
	if _, err := c.trustedURL(address); err != nil {
		return nil, err
	}
	if assetOnly {
		if _, err := c.trustedAssetURL(address); err != nil {
			return nil, err
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	baseClient := c.httpClient()
	requestClient := *baseClient
	requestClient.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		if assetOnly {
			_, err := c.trustedAssetURL(request.URL.String())
			return err
		}
		_, err := c.trustedURL(request.URL.String())
		return err
	}
	response, err := requestClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if _, err := c.trustedURL(response.Request.URL.String()); err != nil {
		return nil, err
	}
	if assetOnly {
		if _, err := c.trustedAssetURL(response.Request.URL.String()); err != nil {
			return nil, err
		}
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("request returned %s", response.Status)
	}
	reader := io.LimitReader(response.Body, limit+1)
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("download exceeds %d-byte limit", limit)
	}
	return data, nil
}

func (c *Client) trustedAssetBase(raw string) (string, error) {
	parsed, err := c.trustedURL(raw)
	if err != nil {
		return "", fmt.Errorf("invalid asset base: %w", err)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", fmt.Errorf("asset base must not contain a path, query, or fragment")
	}
	if !c.assetHosts()[strings.ToLower(parsed.Hostname())] {
		return "", fmt.Errorf("untrusted asset host %q", parsed.Hostname())
	}
	return strings.TrimSuffix(parsed.String(), "/"), nil
}

func (c *Client) trustedAssetURL(raw string) (string, error) {
	parsed, err := c.trustedURL(raw)
	if err != nil {
		return "", err
	}
	if !c.assetHosts()[strings.ToLower(parsed.Hostname())] {
		return "", fmt.Errorf("untrusted asset host %q", parsed.Hostname())
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("asset URL must not contain a query or fragment")
	}
	cleaned := path.Clean(parsed.Path)
	escaped := strings.ToLower(parsed.EscapedPath())
	if !allowedAssetPath(cleaned) || cleaned != parsed.Path || strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%5c") || strings.Contains(parsed.Path, "\\") {
		return "", fmt.Errorf("asset path is outside the supported catalog roots")
	}
	return parsed.String(), nil
}

func (c *Client) resolveAssetURL(assetBase, reference string) (string, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return "", fmt.Errorf("asset path is empty")
	}
	if parsed, err := url.Parse(reference); err == nil && parsed.IsAbs() {
		return c.trustedAssetURL(reference)
	}
	parsed, err := url.Parse(reference)
	if err != nil || parsed.Host != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("invalid relative asset path")
	}
	cleaned := path.Clean("/" + strings.TrimPrefix(parsed.Path, "/"))
	if !allowedAssetPath(cleaned) || strings.Contains(parsed.Path, "\\") {
		return "", fmt.Errorf("asset path is outside the supported catalog roots")
	}
	return c.trustedAssetURL(strings.TrimSuffix(assetBase, "/") + cleaned)
}

func allowedAssetPath(value string) bool {
	return strings.HasPrefix(value, "/pets/") || strings.HasPrefix(value, "/community/") || strings.HasPrefix(value, "/curated/")
}

func (c *Client) assetHosts() map[string]bool {
	if c.TrustedAssetHosts != nil {
		return c.TrustedAssetHosts
	}
	return map[string]bool{TrustedAssetHost: true}
}

func (c *Client) trustedURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() == "" {
		return nil, fmt.Errorf("URL must use HTTPS without credentials")
	}
	hosts := c.TrustedHosts
	if hosts == nil {
		hosts = map[string]bool{"petdex.dev": true, TrustedAssetHost: true}
	}
	if !hosts[strings.ToLower(parsed.Hostname())] {
		return nil, fmt.Errorf("untrusted host %q", parsed.Hostname())
	}
	return parsed, nil
}

func decodePreview(data []byte) (*Animation, error) {
	imageValue, err := decodeImage(data)
	if err != nil {
		return nil, err
	}
	return PreviewAnimation(imageValue)
}

func previewCacheName(entry Entry) string {
	digest := sha256.Sum256([]byte(entry.SpritesheetURL))
	return fmt.Sprintf("%s-%x.webp", entry.Slug, digest[:6])
}

func decodeImage(data []byte) (image.Image, error) {
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode image header: %w", err)
	}
	if config.Width < 1 || config.Height < 1 || config.Width > maxImageSide || config.Height > maxImageSide || int64(config.Width)*int64(config.Height) > maxImagePixels {
		return nil, fmt.Errorf("image dimensions %dx%d exceed limits", config.Width, config.Height)
	}
	imageValue, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	return imageValue, nil
}

func (c *Client) cacheFile(target string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	return installtxn.WriteFileAtomically(target, data, 0o600)
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	client := &http.Client{Timeout: 20 * time.Second}
	return client
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) cacheDir() string     { return filepath.Join(c.RootDir, "pets", "cache") }
func (c *Client) previewDir() string   { return filepath.Join(c.cacheDir(), "previews") }
func (c *Client) installedDir() string { return filepath.Join(c.RootDir, "pets", "installed") }

func validateSlug(slug string) error {
	if slug == "" || len(slug) > 128 {
		return fmt.Errorf("invalid pet slug %q", slug)
	}
	for index, value := range slug {
		if (value >= 'a' && value <= 'z') || (value >= '0' && value <= '9') || (index > 0 && (value == '-' || value == '_')) {
			continue
		}
		return fmt.Errorf("invalid pet slug %q", slug)
	}
	return nil
}

func mergeEntries(local, remote []Entry) []Entry {
	bySlug := make(map[string]Entry, len(local)+len(remote))
	for _, entry := range remote {
		bySlug[entry.Slug] = entry
	}
	for _, entry := range local {
		bySlug[entry.Slug] = entry
	}
	entries := make([]Entry, 0, len(bySlug))
	for _, entry := range bySlug {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Local != entries[j].Local {
			return entries[i].Local
		}
		return strings.ToLower(entries[i].Label()) < strings.ToLower(entries[j].Label())
	})
	return entries
}

func sortEntriesByRanking(entries []Entry, ranking []string) {
	rank := make(map[string]int, len(ranking))
	for index, slug := range ranking {
		rank[slug] = index
	}
	sort.SliceStable(entries, func(i, j int) bool {
		left, right := entries[i], entries[j]
		if left.Local != right.Local {
			return left.Local
		}
		if left.Local {
			return strings.ToLower(left.Label()) < strings.ToLower(right.Label())
		}
		leftRank, leftRanked := rank[left.Slug]
		rightRank, rightRanked := rank[right.Slug]
		if leftRanked != rightRanked {
			return leftRanked
		}
		if leftRanked {
			return leftRank < rightRank
		}
		return strings.ToLower(left.Label()) < strings.ToLower(right.Label())
	})
}
