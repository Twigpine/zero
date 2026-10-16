package dictation

import (
	"archive/tar"
	"cmp"
	"compress/bzip2"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Gitlawb/zero/internal/fsutil"
	"github.com/Gitlawb/zero/internal/lockutil"
)

// Auto-download of the local engine + a default model (opt-in, behind a confirm
// in the F9 flow). Assets come from the official k2-fsa/sherpa-onnx GitHub
// releases and are verified against a SHA256 digest before use — the same trust
// boundary as installing them yourself, automated. Auto-downloading a native
// executable is a real trust decision, which is why it is opt-in and verified.
//
// Rather than freeze a single hardcoded version, the downloader resolves the
// release ASSETS via the GitHub API at download time and verifies each download
// against the API's per-asset digest — so a newer sherpa-onnx release (or
// "latest") works with no code change. A known-good version is still the DEFAULT
// (reproducible, smoke-tested), and for that default the resolved digest is
// cross-checked against a pinned value so a silently-changed release is caught.

// DefaultSherpaVersion is the pinned, smoke-tested release used unless
// stt.engineVersion overrides it. "latest" or any tag also works.
const DefaultSherpaVersion = "v1.13.3"

const (
	sherpaRepo      = "k2-fsa/sherpa-onnx"
	modelReleaseTag = "asr-models" // rolling release the ASR models live in
	modelAssetName  = "sherpa-onnx-moonshine-tiny-en-int8.tar.bz2"
	defaultAPIBase  = "https://api.github.com"
)

// engineAssetSuffix maps GOOS-GOARCH to the version-independent suffix of the
// sherpa-onnx executable bundle for that platform. Matching by suffix (not a
// full version-embedded name) is what lets any release resolve. The bundles are
// self-contained (RPATH $ORIGIN/../lib), so the extracted bin/ executables run
// with no library-path setup.
var engineAssetSuffix = map[string]string{
	"linux-amd64":   "linux-x64-shared-no-tts.tar.bz2",
	"darwin-arm64":  "osx-arm64-shared-no-tts.tar.bz2",
	"darwin-amd64":  "osx-x64-shared-no-tts.tar.bz2",
	"windows-amd64": "win-x64-shared-MT-Release-no-tts.tar.bz2",
}

// pinnedEngineDigest cross-checks the DefaultSherpaVersion engine assets (a
// silently-changed pinned release is refused). Empty for non-default versions,
// which are verified against the API digest only.
var pinnedEngineDigest = map[string]string{
	"linux-amd64":   "e5cbfd96f3666c25d2272d468e337764fce6a4d50ed074e4b8e5021bfd7fefc1",
	"darwin-arm64":  "ea73233bc250019ccf1f8a16feb29460efcd3425b50cac16980bcb76281d60bf",
	"darwin-amd64":  "563423887d63d8a831c473e0ab6815139fe2e7a3482936716c2a128071dd217f",
	"windows-amd64": "343c6c39cb4fe1880a9fb1abc8401d5174629ccbb147fca1fd53c440b2664570",
}

// pinnedModelDigest cross-checks the default Moonshine-tiny int8 model.
const pinnedModelDigest = "d5fe6ec4334fef36255b2a4010412cad4c007e33103fec62fb5d17cad88086f2"

// ModelVariant is a downloadable model the /stt-model picker offers, with a
// pinned SHA256 (the model release predates GitHub's per-asset digests, so these
// must be pinned). Batch variants use the offline binary; the streaming variant
// (a transducer) drives the websocket server for a live transcript.
type ModelVariant struct {
	ID          string
	Label       string
	Description string
	Bytes       int64
	AssetName   string
	Digest      string
	DirName     string
	Streaming   bool
	// Recommended marks a curated, checksum-pinned, validated model (shown first).
	Recommended bool
}

// ModelVariants returns the curated English models the download picker offers.
// This is a hand-picked shortlist (each pinned by SHA256 since the model release
// predates GitHub's per-asset digests), NOT the whole sherpa-onnx model zoo —
// many more models (other languages, sizes, and streaming/batch families) work
// via a manual stt.localModelPath. Streaming (live) options come first.
func ModelVariants() []ModelVariant {
	return []ModelVariant{
		// Live / streaming — transcript builds up as you speak.
		// DirName MUST match what ListModels derives from the asset ("model-"+id, id =
		// asset minus "sherpa-onnx-" prefix and ".tar.bz2" suffix, dates included) so a
		// model extracts to the same directory whether picked from the curated shortlist
		// or the full browse list — otherwise installed-detection disagrees between the
		// two views and the same model can be downloaded twice. Recommended matches the
		// full list too (recommendedModelIDs marks every curated variant), so the ★ and
		// ordering don't change when the background fetch swaps the curated list out.
		{
			ID: "streaming-zipformer-en-kroko", Label: "Kroko (newest, fastest)",
			Description: "smallest live model", Bytes: 54 << 20,
			AssetName:   "sherpa-onnx-streaming-zipformer-en-kroko-2025-08-06.tar.bz2",
			Digest:      "c8676e5ff9ac2a85296e53ee0fd4d5fb1db6770e7a7647166eeafe349ade6834",
			DirName:     "model-streaming-zipformer-en-kroko-2025-08-06",
			Streaming:   true,
			Recommended: true,
		},
		{
			ID: "streaming-zipformer-20m", Label: "Zipformer 20M",
			Description: "small, well-tested", Bytes: 121 << 20,
			AssetName: "sherpa-onnx-streaming-zipformer-en-20M-2023-02-17.tar.bz2",
			Digest:    "9c559283e8498d3fe95913c79ca1cb454bb26281ac2b102b41306c7d752765d9",
			DirName:   "model-streaming-zipformer-en-20M-2023-02-17",
			Streaming: true,
		},
		{
			ID: "streaming-zipformer", Label: "Zipformer",
			Description: "larger, more accurate", Bytes: 297 << 20,
			AssetName: "sherpa-onnx-streaming-zipformer-en-2023-06-26.tar.bz2",
			Digest:    "639e25b578e9e997131402199419c13a941f8e4e198e2da1ce57dbf5cf401282",
			DirName:   "model-streaming-zipformer-en-2023-06-26",
			Streaming: true,
		},
		{
			ID: "nemo-streaming-fast-conformer-transducer-en-80ms-int8", Label: "NeMo streaming (fast)",
			Description: "low-latency English", Bytes: 98 << 20,
			AssetName:   "sherpa-onnx-nemo-streaming-fast-conformer-transducer-en-80ms-int8.tar.bz2",
			Digest:      "7bd33a914e93370a1ba9c2066d9e841bdcad8613fa2a00537c1ae15d851a14d8",
			DirName:     "model-nemo-streaming-fast-conformer-transducer-en-80ms-int8",
			Streaming:   true,
			Recommended: true,
		},
		// Batch — transcribes once when you stop.
		{
			ID: "moonshine-tiny", Label: "Moonshine tiny",
			Description: "fast, smallest", Bytes: 103 << 20,
			AssetName: "sherpa-onnx-moonshine-tiny-en-int8.tar.bz2",
			Digest:    pinnedModelDigest,
			DirName:   "model-moonshine-tiny-en-int8",
		},
		{
			ID: "moonshine-base", Label: "Moonshine base",
			Description: "more accurate, larger", Bytes: 239 << 20,
			AssetName:   "sherpa-onnx-moonshine-base-en-int8.tar.bz2",
			Digest:      "21870cecaa2e44e4e2bf63e02d1072bed183ccd10284871353bd9d24dad14e5e",
			DirName:     "model-moonshine-base-en-int8",
			Recommended: true,
		},
		{
			ID: "sense-voice-zh-en-ja-ko-yue-int8-2025-09-09", Label: "SenseVoice",
			Description: "multilingual, accurate", Bytes: 158 << 20,
			AssetName:   "sherpa-onnx-sense-voice-zh-en-ja-ko-yue-int8-2025-09-09.tar.bz2",
			Digest:      "7305f7905bfcf77fa0b39388a313f3da35c68d971661a65475b56fb2162c8e63",
			DirName:     "model-sense-voice-zh-en-ja-ko-yue-int8-2025-09-09",
			Recommended: true,
		},
		{
			// Heavy (~1 GB) but the best transcription quality — recommended when
			// accuracy matters more than speed/size. The asr-models release publishes
			// no digest for it, so (like every browse-listed model) it downloads with
			// TLS-only verification.
			ID: "whisper-large-v3", Label: "Whisper large v3",
			Description: "best accuracy, heavy", Bytes: 1019 << 20,
			AssetName:   "sherpa-onnx-whisper-large-v3.tar.bz2",
			DirName:     "model-whisper-large-v3",
			Recommended: true,
		},
	}
}

// recommendedModelIDs marks the curated variants flagged Recommended so the full
// browse list stars (and surfaces) exactly the ones the curated shortlist does —
// keyed off the same Recommended flag so the two views never disagree.
var recommendedModelIDs = func() map[string]bool {
	m := map[string]bool{}
	for _, v := range ModelVariants() {
		if v.Recommended {
			m[v.AssetName] = true
		}
	}
	return m
}()

// pinnedModelDigests maps a model asset name to its pinned SHA256 (the curated
// variants), so browse-listed models we happen to know get verified too.
var pinnedModelDigests = func() map[string]string {
	m := map[string]string{}
	for _, v := range ModelVariants() {
		m[v.AssetName] = v.Digest
	}
	return m
}()

// curatedLabels maps a curated model's asset name to its friendly label, so the
// recommended rows read nicely in the full browse list too.
var curatedLabels = func() map[string]string {
	m := map[string]string{}
	for _, v := range ModelVariants() {
		m[v.AssetName] = v.Label
	}
	return m
}()

// browseLabel turns a raw asset id into a shorter display label: it drops the
// redundant "-int8"/quantization noise and the "streaming-" prefix (the group
// header already says live vs batch).
func browseLabel(id string) string {
	label := strings.TrimPrefix(id, "streaming-")
	for _, drop := range []string{"-int8", ".int8", "-quantized", "-fp16"} {
		label = strings.ReplaceAll(label, drop, "")
	}
	return label
}

// ListModels fetches every transcription model in the sherpa-onnx asr-models
// release and returns them as variants (the curated ones flagged Recommended and
// carrying a pinned digest; the rest with an empty digest, downloaded with
// TLS-only verification since that release predates GitHub's per-asset digests).
// Non-ASR assets (VAD, TTS, punctuation, speaker/keyword/…): filtered out.
func ListModels(ctx context.Context, client *http.Client, apiBase string) ([]ModelVariant, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if apiBase == "" {
		apiBase = defaultAPIBase
	}
	// Resolve the release id, then page its assets (a release can have hundreds).
	relURL := fmt.Sprintf("%s/repos/%s/releases/tags/%s", strings.TrimRight(apiBase, "/"), sherpaRepo, modelReleaseTag)
	var rel struct {
		ID     int64 `json:"id"`
		Assets []githubAsset
	}
	if err := getJSON(ctx, client, relURL, &rel); err != nil {
		return nil, fmt.Errorf("listing dictation models: %w", err)
	}
	assets := rel.Assets
	// If the inline assets look capped, page the assets endpoint for the rest.
	for page := 1; len(assets)%100 == 0 && len(assets) > 0; page++ {
		var more []githubAsset
		u := fmt.Sprintf("%s/repos/%s/releases/%d/assets?per_page=100&page=%d", strings.TrimRight(apiBase, "/"), sherpaRepo, rel.ID, page+1)
		if err := getJSON(ctx, client, u, &more); err != nil || len(more) == 0 {
			break
		}
		assets = append(assets, more...)
		if len(more) < 100 {
			break
		}
	}

	seen := map[string]bool{}
	var out []ModelVariant
	for _, a := range assets {
		if seen[a.Name] || !isTranscriptionModelAsset(a.Name) {
			continue
		}
		seen[a.Name] = true
		id := strings.TrimSuffix(strings.TrimPrefix(a.Name, "sherpa-onnx-"), ".tar.bz2")
		digest := strings.TrimPrefix(a.Digest, "sha256:")
		if digest == "" {
			digest = pinnedModelDigests[a.Name] // may still be empty (browse/unverified)
		}
		label := browseLabel(id)
		desc := modelFamily(a.Name) // readable language, e.g. "French"
		if nice, ok := curatedLabels[a.Name]; ok {
			label = nice
			desc = curatedDescriptions[a.Name] // curated blurb ("fast, smallest")
		}
		out = append(out, ModelVariant{
			ID:          id,
			Label:       label,
			Description: desc,
			Bytes:       a.Size,
			AssetName:   a.Name,
			Digest:      digest,
			DirName:     "model-" + id,
			Streaming:   strings.Contains(strings.ToLower(a.Name), "streaming-"),
			Recommended: recommendedModelIDs[a.Name],
		})
	}
	return out, nil
}

type githubAsset struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

func getJSON(ctx context.Context, client *http.Client, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub API HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// isTranscriptionModelAsset reports whether an asset is a speech-to-text model
// (as opposed to a VAD/TTS/punctuation/speaker/keyword/enhancement tool).
func isTranscriptionModelAsset(name string) bool {
	n := strings.ToLower(name)
	if !strings.HasPrefix(n, "sherpa-onnx-") || !strings.HasSuffix(n, ".tar.bz2") {
		return false
	}
	for _, bad := range []string{
		"-vad", "vad-", "tts", "punct", "speaker", "diariz", "kws", "keyword",
		"audio-tagging", "denois", "source-separation", "speech-enhancement",
		"spoken-language", "language-identification", "-lid", "melo", "kokoro",
		"vits", "matcha", "piper", "wespeaker",
		// Rockchip NPU (RKNN) builds for specific ARM boards — not CPU/ONNX, so
		// the standard sherpa-onnx binary can't run them.
		"rknn", "rk3566", "rk3568", "rk3562", "rk3588", "rk3576", "rk3528", "rk3399",
	} {
		if strings.Contains(n, bad) {
			return false
		}
	}
	for _, fam := range []string{
		"zipformer", "moonshine", "whisper", "paraformer", "sense-voice",
		"sensevoice", "conformer", "nemo", "canary", "parakeet", "fire-red",
		"fireredasr", "dolphin", "telespeech", "wenet", "omnilingual", "funasr",
		"medasr", "cohere", "qwen", "citrinet", "transducer", "-ctc-", "zipvoice",
	} {
		if strings.Contains(n, fam) {
			return true
		}
	}
	return false
}

// langNames maps ISO-ish language tokens in a model name to readable names.
var langNames = map[string]string{
	"en": "English", "zh": "Chinese", "yue": "Cantonese", "fr": "French",
	"de": "German", "es": "Spanish", "ru": "Russian", "ja": "Japanese",
	"ko": "Korean", "bn": "Bengali", "ar": "Arabic", "hi": "Hindi",
	"pt": "Portuguese", "nl": "Dutch", "vi": "Vietnamese", "th": "Thai",
	"uk": "Ukrainian", "fa": "Persian", "pl": "Polish", "tr": "Turkish",
	"cs": "Czech", "sv": "Swedish", "fil": "Filipino", "tl": "Tagalog",
	"fi": "Finnish", "el": "Greek", "he": "Hebrew", "ro": "Romanian",
}

// modelFamily is a short readable descriptor for a model, favoring the language
// (the most useful thing not already obvious from the group header) and falling
// back to the family. E.g. "French", "Chinese", "Multilingual".
func modelFamily(name string) string {
	n := strings.ToLower(name)
	if strings.Contains(n, "multi") || strings.Contains(n, "bilingual") ||
		strings.Contains(n, "trilingual") || strings.Contains(n, "polyglot") ||
		strings.Contains(n, "omnilingual") {
		return "Multilingual"
	}
	var langs []string
	seen := map[string]bool{}
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			langs = append(langs, name)
		}
	}
	for _, tok := range strings.Split(strings.TrimSuffix(n, ".tar.bz2"), "-") {
		add(langNames[tok])
	}
	// Full language words spelled out in some names (e.g. "-cantonese-").
	for word, name := range map[string]string{
		"cantonese": "Cantonese", "mandarin": "Chinese", "english": "English",
		"chinese": "Chinese", "korean": "Korean", "japanese": "Japanese",
		"russian": "Russian", "arabic": "Arabic", "spanish": "Spanish",
		"french": "French", "german": "German", "vietnamese": "Vietnamese",
		"thai": "Thai", "farsi": "Persian", "persian": "Persian",
		"portuguese": "Portuguese", "italian": "Italian", "dutch": "Dutch",
		"ukrainian": "Ukrainian", "polish": "Polish", "turkish": "Turkish",
	} {
		if strings.Contains(n, word) {
			add(name)
		}
	}
	switch {
	case len(langs) == 0:
		return ""
	case len(langs) > 2:
		return "Multilingual"
	default:
		return strings.Join(langs, " + ")
	}
}

// curatedDescriptions maps a curated model's asset name to its quality blurb.
var curatedDescriptions = func() map[string]string {
	m := map[string]string{}
	for _, v := range ModelVariants() {
		m[v.AssetName] = v.Description
	}
	return m
}()

// engineTagDigestChars is how much of the raw tag's SHA256 an encoded name
// carries. Enough that two tags cannot be brought to the same destination by
// picking their punctuation, short enough that the name stays readable.
const engineTagDigestChars = 12

// safeInstallTag reports whether a release tag is already one safe path
// component: letters, digits, and the three separators a version string
// actually uses. Anything else (a '/' above all, but also a ':' or a backslash
// Windows would read as one) has to be encoded rather than joined.
func safeInstallTag(tag string) bool {
	if tag == "" {
		return false
	}
	for i := 0; i < len(tag); i++ {
		if !safeInstallTagByte(tag[i]) {
			return false
		}
	}
	return true
}

// safeInstallTagByte is the safe set itself, one byte at a time, so the check
// and the sanitizer below cannot drift into disagreeing about which bytes are
// the ones that have to be encoded.
func safeInstallTagByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '.', c == '_', c == '-':
		return true
	}
	return false
}

// engineDestName is an engine install's identity: one path component, derived
// from a release tag that can be any string a release is named. The name is
// also the Install lock's key and the destination recovery reads out of a
// holder marker, so a separator in it would have the lock file, the marker, and
// the install itself disagree about which object is protected. A tag that is
// already safe encodes to itself byte for byte, which is what keeps every
// engine already on disk under the name it has; anything else is sanitized and
// carries a digest of the RAW tag, so two tags that sanitize alike still name
// two destinations.
func engineDestName(version, key string) string {
	if safeInstallTag(version) {
		return "engine-" + version + "-" + key
	}
	sanitized := []byte(version)
	for i, c := range sanitized {
		if !safeInstallTagByte(c) {
			sanitized[i] = '_'
		}
	}
	sum := sha256.Sum256([]byte(version))
	return "engine-" + string(sanitized) + "-" + hex.EncodeToString(sum[:])[:engineTagDigestChars] + "-" + key
}

// longestPlatformKey is the widest GOOS-GOARCH key an engine destination can
// carry, read off the asset table so adding a platform cannot widen the name
// past the bound below without widening the bound with it.
var longestPlatformKey = func() int {
	n := 0
	for key := range engineAssetSuffix {
		n = max(n, len(key))
	}
	return n
}()

// installNameMax is the bytes a single filename gets on the filesystems this
// runs on (NAME_MAX is 255 on Linux and macOS; a Windows path component is
// bounded well above anything built here).
const installNameMax = 255

// maxInstallTagBytes bounds a release tag so BOTH the destination it encodes to
// and that destination's WIDEST SIBLING are names the filesystem will accept.
// The sibling is the tighter of the two, and that is why this is a rule rather
// than arithmetic: a tag can be short enough for a legal install directory and
// a legal "<name>.lock" and still push the sequenced holder promotion sets the
// install aside in past the limit, and the error that comes back then names an
// internal holder path the user never chose, after the download and extraction
// already ran, with nothing in it pointing at the setting to change. The
// budget subtracts every byte an encoded name adds around the tag: the prefix,
// the digest and its separator, the separator before the platform key, the
// widest platform key, and the widest suffix a sibling of the destination can
// take — the lock file, the ".previous-<seq>-seq" holder, and the ".kept-<seq>
// -seq" copy recovery retains.
var maxInstallTagBytes = installNameMax -
	(len("engine-") + 1 + engineTagDigestChars + 1 + longestPlatformKey +
		max(len(".lock"), len(holderSuffix)+holderSeqDigits+len(holderSeqSuffix),
			len(keptSuffix)+holderSeqDigits+len(holderSeqSuffix)))

// EngineComponents identifies the resolved local-engine paths.
type EngineComponents struct {
	BinaryPath string // extracted sherpa-onnx-offline
	ServerPath string // extracted sherpa-onnx-online-websocket-server
	ModelPath  string // extracted model directory
}

// DownloadOptions configures EnsureLocalEngine.
type DownloadOptions struct {
	// DestRoot is where extracted engines/models live (e.g. ~/.config/zero/stt).
	DestRoot string
	// EngineVersion selects the sherpa-onnx release tag ("" → DefaultSherpaVersion;
	// "latest" and any tag also work).
	EngineVersion string
	// HTTPClient is injectable for tests; nil uses a plain client.
	HTTPClient *http.Client
	// APIBase overrides the GitHub API base (tests). "" → api.github.com.
	APIBase string
	// Model selection (default: the Moonshine-tiny int8 batch model). Set these
	// from a ModelVariant to download a different model.
	ModelAssetName    string
	ModelPinnedDigest string
	ModelDirName      string
	// ModelLabel is the friendly model name shown in progress ("Zipformer 20M").
	ModelLabel string
	// Progress receives live status strings for the UI, including a percentage
	// while downloading (e.g. "Engine 45% · 57/126 MB").
	Progress func(status string)
	// platformKey overrides runtime detection (tests).
	platformKey string
	// skipPinned disables the pinned-digest cross-check (tests, which use fixture
	// assets); the API-digest verification still runs.
	skipPinned bool
}

// AutoDownloadSupported reports whether the current platform has a known engine
// asset (so the F9 flow can offer the download). Termux/Android and unusual
// arches install manually or use a cloud provider.
func AutoDownloadSupported() bool {
	_, ok := engineAssetSuffix[platformKey()]
	return ok
}

func platformKey() string { return runtime.GOOS + "-" + runtime.GOARCH }

// EnsureLocalEngine downloads (if absent) and verifies the sherpa-onnx engine and
// the default model into DestRoot, returning the resolved paths. Idempotent: an
// already-extracted, still-present engine/model is reused without re-downloading.
// Every archive is SHA256-verified against the GitHub API digest before extraction.
func EnsureLocalEngine(ctx context.Context, opts DownloadOptions) (EngineComponents, error) {
	if strings.TrimSpace(opts.DestRoot) == "" {
		return EngineComponents{}, fmt.Errorf("dictation download: DestRoot is required")
	}
	key := opts.platformKey
	if key == "" {
		key = platformKey()
	}
	suffix, ok := engineAssetSuffix[key]
	if !ok {
		return EngineComponents{}, &SetupError{
			Tool: "sherpa-onnx auto-download",
			Hint: fmt.Sprintf("no prebuilt engine is known for %s — install sherpa-onnx manually or use a cloud provider (see docs/dictation.md)", key),
		}
	}
	version := strings.TrimSpace(opts.EngineVersion)
	if version == "" {
		version = DefaultSherpaVersion
	}
	// Bounded here, where the tag is still the thing being talked about. Past
	// this point it is a destination name and then a lock filename, and the
	// filesystem refuses the lock file before it refuses the directory: the
	// error that surfaces then names lockutil and a path the user never chose.
	if len(version) > maxInstallTagBytes {
		return EngineComponents{}, &SetupError{
			Tool: "sherpa-onnx auto-download",
			Hint: fmt.Sprintf("the release tag %q is %d bytes, and a tag must be at most %d so the install directory and its lock file are names the filesystem accepts — shorten stt.engineVersion", version, len(version), maxInstallTagBytes),
		}
	}
	progress := opts.Progress
	if progress == nil {
		progress = func(string) {}
	}
	client := opts.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	apiBase := opts.APIBase
	if apiBase == "" {
		apiBase = defaultAPIBase
	}

	engineName := engineDestName(version, key)
	engineDir := filepath.Join(opts.DestRoot, engineName)
	targetWindows := strings.HasPrefix(key, "windows-")
	// Resolve through the tarball's flattened subdir so an ALREADY-extracted
	// engine is found and not needlessly re-downloaded (the idempotency check).
	enginePublished := func(dir string) (bool, error) {
		bin, _, err := resolveEnginePathsErr(dir, targetWindows)
		if err != nil {
			return false, err
		}
		return fileExistsErr(bin)
	}
	// Released versions joined the raw tag to DestRoot, so a tag that is not one
	// path component extracted into a nested directory. That copy is a real
	// engine and may be the only one on the disk, so it is reused rather than
	// downloaded again beside a name it would never be found under. Nothing is
	// installed when it is, which is why this runs outside the Install lock: the
	// lock covers a destination this call then does not touch.
	installed := false
	if rawDir, ok := compatEngineDir(opts.DestRoot, "engine-"+version+"-"+key); ok && rawDir != engineDir {
		// Best-effort on purpose: this lookup only ever REUSES a copy, and its
		// own comment says nothing is installed when it hits. A probe that
		// could not run there (a stray file, an unreadable directory, a tag no
		// stat can even take) is therefore no reason to fail a call whose real
		// destination may already hold a working engine, and failing on it
		// leaves a permanent error the user can only clear by editing config.
		if published, err := usability(enginePublished).check(rawDir); err == nil && published {
			engineDir = rawDir
			installed = true
		}
	}
	// The engine lock covers recovery, the decision to download, and the
	// promotion, and it is released before the model lock is taken. Holding
	// both would give two concurrent installs of different models an ordering
	// to get wrong for no gain.
	if !installed {
		if err := withDestinationLock(ctx, opts.DestRoot, engineName, enginePublished, func(txn *destTxn) error {
			// A previous run may have been stopped mid-promotion, leaving the only
			// install in a holder beside engineDir. Put it back before deciding
			// whether anything needs downloading.
			// A recovery that stopped on a state it could not read has ruled on
			// nothing. Deciding anything past this point would read "no evidence"
			// as "nothing to recover" and download over an install that is sitting
			// in a holder beside the destination.
			if err := restoreInterruptedPromotion(txn, engineDir, enginePublished, progress); err != nil {
				return err
			}
			published, err := usability(enginePublished).check(engineDir)
			if err != nil {
				return err
			}
			if published {
				return nil
			}
			pinned := ""
			if version == DefaultSherpaVersion && !opts.skipPinned {
				pinned = pinnedEngineDigest[key]
			}
			asset, err := resolveAsset(ctx, client, apiBase, version, "sherpa-onnx-", suffix)
			if err != nil {
				return err
			}
			return downloadVerifyExtract(ctx, client, asset, pinned, false, "Engine", engineDir, txn, progress)
		}); err != nil {
			return EngineComponents{}, err
		}
	}
	binPath, serverPath, err := resolveEnginePathsErr(engineDir, targetWindows)
	if err != nil {
		return EngineComponents{}, err
	}
	if !fileExists(binPath) {
		return EngineComponents{}, fmt.Errorf("dictation download: engine binary not found after extraction in %s", engineDir)
	}

	modelName := opts.ModelAssetName
	if modelName == "" {
		modelName = modelAssetName
	}
	modelDirName := opts.ModelDirName
	if modelDirName == "" {
		modelDirName = "model-moonshine-tiny-en-int8"
	}
	modelDir := filepath.Join(opts.DestRoot, modelDirName)
	if err := withDestinationLock(ctx, opts.DestRoot, modelDirName, dirHasModel, func(txn *destTxn) error {
		// promoteStagedDir is shared with the model, so a stop mid-promotion
		// leaves the model in a holder too. Put it back before deciding
		// anything is missing: without this an offline user has no download to
		// fall back on.
		if err := restoreInterruptedPromotion(txn, modelDir, dirHasModel, progress); err != nil {
			return err
		}
		present, err := usability(dirHasModel).check(modelDir)
		if err != nil {
			return err
		}
		if present {
			return nil
		}
		asset, err := resolveAsset(ctx, client, apiBase, modelReleaseTag, modelName, "")
		if err != nil {
			return err
		}
		modelPinned := opts.ModelPinnedDigest
		// Only fall back to the built-in digest for the DEFAULT model — a different
		// (browse-listed) model must never be checked against the default's digest.
		if modelPinned == "" && opts.ModelAssetName == "" {
			modelPinned = pinnedModelDigest
		}
		if opts.skipPinned {
			modelPinned = ""
		}
		modelLabel := opts.ModelLabel
		if modelLabel == "" {
			modelLabel = "Model"
		}
		// Models are data files from the official release: verify against a digest
		// when we have one (curated/pinned or API-provided), else allow a TLS-only
		// download (the model release predates GitHub's per-asset digests). The
		// engine BINARY above never allows this — a native executable is always
		// digest-verified.
		return downloadVerifyExtract(ctx, client, asset, modelPinned, true, modelLabel, modelDir, txn, progress)
	}); err != nil {
		return EngineComponents{}, err
	}
	resolvedModel := modelDir
	present, presentErr := hasTokensFile(resolvedModel)
	if presentErr != nil && !errors.Is(presentErr, fs.ErrNotExist) {
		return EngineComponents{}, presentErr
	}
	if !present {
		if child, err := flattenSingleChild(modelDir); err == nil {
			resolvedModel = child
		}
		present, presentErr = hasTokensFile(resolvedModel)
		if presentErr != nil && !errors.Is(presentErr, fs.ErrNotExist) {
			return EngineComponents{}, presentErr
		}
	}
	if !present {
		return EngineComponents{}, fmt.Errorf("dictation download: model files not found after extraction in %s", modelDir)
	}
	return EngineComponents{BinaryPath: binPath, ServerPath: serverPath, ModelPath: resolvedModel}, nil
}

// compatEngineDir resolves the pre-encoding layout's directory for a raw
// release tag and reports whether it is one this install may adopt. The tag is
// any string a release is named and filepath.Join CLEANS, so a tag carrying
// ".." resolves outside DestRoot entirely: the tree found there would become
// engineDir, with no install lock and no digest behind it, and its bin/ is what
// the dictation runner executes. The nested case is the real one this lookup
// exists for (a tag with a separator extracted to DestRoot/engine-release/
// v1-<key>), so the rule is containment under DestRoot rather than a single
// path component. Lstat, not Stat: Stat follows a link, and a symlink planted
// at the raw path points wherever it likes, adopting content from outside the
// root through a path that passes the containment check.
func compatEngineDir(destRoot, rawName string) (string, bool) {
	root := filepath.Clean(destRoot)
	dir := filepath.Clean(filepath.Join(root, rawName))
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	info, err := holderFS.lstat(dir)
	if err != nil || !info.IsDir() {
		return "", false
	}
	return dir, true
}

type resolvedAsset struct {
	url    string
	sha256 string
	size   int64
	name   string
}

// resolveAsset queries the GitHub release for tag and returns the asset whose
// name has the given prefix and suffix, plus its SHA256 digest and size.
func resolveAsset(ctx context.Context, client *http.Client, apiBase, tag, namePrefix, nameSuffix string) (resolvedAsset, error) {
	tagPath := "tags/" + tag
	if tag == "" || tag == "latest" {
		tagPath = "latest"
	}
	url := fmt.Sprintf("%s/repos/%s/releases/%s", strings.TrimRight(apiBase, "/"), sherpaRepo, tagPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return resolvedAsset{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return resolvedAsset{}, fmt.Errorf("resolving sherpa-onnx release %s: %w", tag, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resolvedAsset{}, fmt.Errorf("resolving sherpa-onnx release %s: GitHub API HTTP %d", tag, resp.StatusCode)
	}
	var release struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name        string `json:"name"`
			DownloadURL string `json:"browser_download_url"`
			Digest      string `json:"digest"`
			Size        int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return resolvedAsset{}, fmt.Errorf("parsing sherpa-onnx release %s: %w", tag, err)
	}
	for _, a := range release.Assets {
		if !strings.HasPrefix(a.Name, namePrefix) || !strings.HasSuffix(a.Name, nameSuffix) {
			continue
		}
		// The API digest may be empty for older assets (GitHub only populates it for
		// assets uploaded after ~2024 — the ASR models predate that). An empty
		// digest is fine here; downloadVerifyExtract requires the API digest OR a
		// pinned one and refuses only when neither exists.
		digest := strings.TrimPrefix(a.Digest, "sha256:")
		return resolvedAsset{url: a.DownloadURL, sha256: digest, size: a.Size, name: a.Name}, nil
	}
	return resolvedAsset{}, &SetupError{
		Tool: "sherpa-onnx auto-download",
		Hint: fmt.Sprintf("release %s has no asset matching %s*%s — try a different stt.engineVersion or install manually", tag, namePrefix, nameSuffix),
	}
}

// downloadVerifyExtract fetches the asset to a temp file, verifies its SHA256
// (against the API digest, and — when pinned is set — a cross-check that the
// resolved digest equals the audited value), and extracts the tar.bz2. A
// mismatch aborts before anything is extracted or run.
func downloadVerifyExtract(ctx context.Context, client *http.Client, asset resolvedAsset, pinned string, allowUnverified bool, label, destDir string, txn *destTxn, progress func(string)) error {
	// Refused here as well as in promoteStagedDir, so a caller that forgot the
	// lock does not get a full download and extraction before finding out.
	if !txn.holds(destDir) {
		return fmt.Errorf("dictation download: refusing to install %s: no install lock is held for %s", label, destDir)
	}
	// Determine the digest to verify against: the API digest and the pinned digest
	// must agree when both exist; at least one must exist unless allowUnverified
	// (models are data files from the official release — TLS-only is acceptable;
	// a native executable never sets allowUnverified).
	expected := asset.sha256
	if pinned != "" {
		if expected != "" && !strings.EqualFold(expected, pinned) {
			return fmt.Errorf("dictation download: %s digest %s does not match the pinned known-good value %s — refusing (the release may have changed)", asset.name, asset.sha256, pinned)
		}
		expected = pinned
	}
	if expected == "" && !allowUnverified {
		return fmt.Errorf("dictation download: no SHA256 available for %s (neither the GitHub API nor a pinned value) — refusing an unverifiable download", asset.name)
	}
	// Ensure the parent of destDir exists; the final destDir is created by
	// the atomic rename at the end of the function.
	if err := os.MkdirAll(filepath.Dir(destDir), 0o755); err != nil {
		return err
	}
	// Stage the extraction into a sibling temp dir and only swap it into
	// destDir on success. An interrupted run that left a truncated engine
	// binary in destDir would otherwise pass fileExists() on the next start
	// and break the local recorder silently.
	stageDir, err := os.MkdirTemp(filepath.Dir(destDir), filepath.Base(destDir)+".stage-*")
	if err != nil {
		return err
	}
	cleanupStage := true
	defer func() {
		if cleanupStage {
			_ = os.RemoveAll(stageDir)
		}
	}()

	tmp, err := os.CreateTemp(filepath.Dir(destDir), "stt-dl-*.tar.bz2")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.url, nil)
	if err != nil {
		tmp.Close()
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		tmp.Close()
		return fmt.Errorf("downloading %s: %w", asset.name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		tmp.Close()
		return fmt.Errorf("downloading %s: HTTP %d", asset.name, resp.StatusCode)
	}
	hasher := sha256.New()
	total := asset.size
	if total <= 0 {
		total = resp.ContentLength
	}
	src := io.Reader(resp.Body)
	if total > 0 {
		src = &progressReader{r: resp.Body, total: total, label: label, report: progress, lastPct: -1}
	}
	if _, err := io.Copy(io.MultiWriter(tmp, hasher), src); err != nil {
		tmp.Close()
		return fmt.Errorf("downloading %s: %w", asset.name, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	if expected != "" && !strings.EqualFold(got, expected) {
		return fmt.Errorf("dictation download: checksum mismatch for %s (want %s, got %s) — refusing to extract", asset.name, expected, got)
	}
	if err := extractTarBz2(tmpPath, stageDir, "Extracting "+label, progress); err != nil {
		return err
	}
	if err := promoteStagedDir(txn, stageDir, destDir, label, progress); err != nil {
		return err
	}
	cleanupStage = false
	return nil
}

// progressReader reports download progress as a percentage on each whole-percent
// change, formatted "<label> NN% · X/Y MB".
type progressReader struct {
	r       io.Reader
	total   int64
	read    int64
	label   string
	report  func(string)
	lastPct int
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.read += int64(n)
	if p.total > 0 {
		pct := int(p.read * 100 / p.total)
		if pct != p.lastPct {
			p.lastPct = pct
			p.report(fmt.Sprintf("%s %d%% · %d/%d MB", p.label, pct, p.read>>20, p.total>>20))
		}
	}
	return n, err
}

// enginePaths builds the engine executable paths. The ".exe" suffix follows the
// TARGET platform (the one the engine was downloaded for), not the host — they
// match in production (you download your own platform), but keying off the target
// keeps resolution correct when the two differ (e.g. a cross-platform test).
func enginePaths(engineDir string, targetWindows bool) (bin, server string) {
	ext := ""
	if targetWindows {
		ext = ".exe"
	}
	return filepath.Join(engineDir, "bin", "sherpa-onnx-offline"+ext),
		filepath.Join(engineDir, "bin", "sherpa-onnx-online-websocket-server"+ext)
}

// resolveEnginePaths finds the executables, flattening the tarball's single
// top-level directory when present. The boolean form: callers that already know
// the tree is readable, and tests.
func resolveEnginePaths(engineDir string, targetWindows bool) (bin, server string) {
	bin, server, err := resolveEnginePathsErr(engineDir, targetWindows)
	if err != nil {
		return enginePaths(engineDir, targetWindows)
	}
	return bin, server
}

// resolveEnginePathsErr is the error-aware form, and it is what the publication
// probe resolves through. Dropping these errors is how a tree nobody could read
// gets a DEFINITE answer: a stat that fails with anything but "not there" falls
// through to the flattened child, the child holds no binary, and the probe
// reports "not installed" about an install that is sitting right there. The
// caller then downloads over it and its deferred cleanup reaps it. Only
// fs.ErrNotExist is an answer here; every other failure is the probe saying
// nothing at all.
func resolveEnginePathsErr(engineDir string, targetWindows bool) (bin, server string, err error) {
	bin, server = enginePaths(engineDir, targetWindows)
	present, err := fileExistsErr(bin)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", "", err
	}
	if present {
		return bin, server, nil
	}
	child, err := flattenSingleChild(engineDir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return "", "", err
		}
	} else if child != engineDir {
		bin, server = enginePaths(child, targetWindows)
	}
	return bin, server, nil
}

// holderSuffix is what promoteStagedDir appends to an install's own name for the
// holder it sets that install aside in. An ordering number goes in the name
// because recovery has to pick the NEWEST holder when a failed cleanup left an
// older one behind, and nothing else records that order: Glob sorts lexically
// and a directory mtime tracks the install's contents, not its promotion. The
// number is a per-install sequence, one past the highest already beside this
// install, so no decision here reads a clock. There is no older shape to accept:
// released versions staged under .stage-* and published with a plain rename,
// and none of them ever created a holder.
// fsOps is the filesystem seam the promotion path, the holder allocator, and
// recovery take every step through. A step that calls os directly cannot be made
// to fail, and the crash each of these steps exists to survive is then only
// reasoned about. Every field is one call, so a test can fail the second remove
// or the rename whose source is one particular holder and leave the rest real.
type fsOps struct {
	rename     func(from, to string) error
	removeAll  func(path string) error
	stat       func(name string) (os.FileInfo, error)
	lstat      func(name string) (os.FileInfo, error)
	readDir    func(name string) ([]os.DirEntry, error)
	readFile   func(name string) ([]byte, error)
	mkdir      func(name string, perm os.FileMode) error
	writeFile  func(name string, data []byte, perm os.FileMode) error
	create     func(name string, flag int, perm os.FileMode) (*os.File, error)
	createTemp func(dir, pattern string) (*os.File, error)
}

// holderFS is the seam every filesystem step in the promotion path goes through.
// Tests swap a field and restore it; nothing else writes to it.
var holderFS = fsOps{
	rename:     os.Rename,
	removeAll:  os.RemoveAll,
	stat:       os.Stat,
	lstat:      os.Lstat,
	readDir:    os.ReadDir,
	readFile:   os.ReadFile,
	mkdir:      os.Mkdir,
	writeFile:  os.WriteFile,
	create:     os.OpenFile,
	createTemp: os.CreateTemp,
}

// installFileLock is the part of lockutil.FileLock this file uses. Release joins
// the platform unlock and the close, and neither can be made to fail through a
// real lock, so without a seam the one path that reports a cleanup failure could
// only be reasoned about.
type installFileLock interface{ Release() error }

// acquireInstallLock is the seam a destination's Install lock is taken through.
// Tests swap it and restore it; nothing else writes to it.
var acquireInstallLock = func(root, path string) (installFileLock, error) {
	lock, err := lockutil.TryAcquireFileLockAt(root, path)
	if err != nil {
		// A typed nil in the interface would be non-nil to every caller, so the
		// failure is returned as an untyped nil lock.
		return nil, err
	}
	return lock, nil
}

// errInstalledNotReleased reports the one failure an operation can have after
// its own mutation already landed: the Install lock is released last, so a
// release failure is all that is left to go wrong. A caller that read it as a
// plain failure would repeat a step that already ran, downloading an engine
// that is installed or re-running a removal that is done, so the error says
// both facts and every caller that can act on the difference tests for it.
var ErrInstalledNotReleased = errors.New("dictation download: the operation completed but its install lock was not released")

// errInstalledNotReleased is the name this package uses internally. Exported
// above because the two callers that decide what a user is told, the kept-backup
// command and the dictation setup screen, have to tell a completed operation
// from one that did not happen, and only the sentinel carries that.
var errInstalledNotReleased = ErrInstalledNotReleased

// errInstallInProgress reports that another process held this destination's
// Install lock for the whole wait budget and the destination is still not
// usable. It is not an install failure: nothing was attempted, and the caller
// reports it and moves on.
var errInstallInProgress = errors.New("another install of this component is already running")

// installLockWait bounds how long a caller waits for another process to finish
// with a destination. It covers a download, so a budget shorter than the
// critical section it guards is a scheduled failure. A var so a test can shorten
// it; nothing else assigns to it.
var installLockWait = 2 * time.Minute

// installLockPoll is the retry interval while waiting. The lock is advisory and
// held by an open file handle, so there is nothing to wake us and polling is how
// the wait ends.
const installLockPoll = 50 * time.Millisecond

// installLockDir holds one lock file per destination. It is a sibling of the
// destinations rather than a file beside each one, so a lock file is never
// mistaken for part of an install and never moves with one.
const installLockDir = ".install-locks"

// destTxn is the handle proving its holder owns the Install lock for one
// destination. Recovery and promotion move and delete whole installs, so both
// take one: without it a second process can delete the copy this one is about
// to roll back to.
type destTxn struct {
	root string
	dest string
	lock installFileLock
	// released is atomic because holds is the guard every locked entry point
	// consults, and a handle can be released on one goroutine while another is
	// still asking whether it is safe to act.
	released atomic.Bool
}

// destDir is the destination this handle locks. Callers keep passing their own
// destDir alongside the handle so a handle for a DIFFERENT destination is
// representable, which is what makes the mismatch testable rather than assumed.
func (t *destTxn) destDir() string { return filepath.Join(t.root, t.dest) }

// holds reports whether this handle actually locks destDir. A nil handle and a
// handle for another destination both fail closed.
func (t *destTxn) holds(destDir string) bool {
	return t != nil && t.lock != nil && !t.released.Load() && t.destDir() == destDir
}

// release drops the Install lock and reports what releasing it cost. Release
// joins the platform unlock and the close, and a failure in either can mean the
// lock file is still held, so the caller merges it into its own outcome rather
// than reporting a clean run over a lock nobody let go of. Idempotent, so a
// caller can release early and still defer it; the flag flips in the same step
// that yields the error, so no caller both fails to release and reports a clean
// release, and holds stops claiming the destination either way.
func (t *destTxn) release() error {
	if t == nil || t.lock == nil {
		return nil
	}
	if t.released.Swap(true) {
		return nil
	}
	return t.lock.Release()
}

// mergeReleaseFailure folds an Install lock release failure into an operation's
// own outcome. done says the operation's mutation already landed, which the
// error has to keep saying: a caller that read this as "nothing happened" would
// repeat a step that already ran.
func mergeReleaseFailure(err, relErr error, done bool, dest string) error {
	if relErr == nil {
		return err
	}
	if err != nil {
		// The primary cause stays the wrapped one: it is what the operation
		// failed on, and the lock is a second fact about the same run.
		return fmt.Errorf("%w (releasing the install lock on %s also failed: %v)", err, dest, relErr)
	}
	if done {
		return fmt.Errorf("%w: %s: %v", errInstalledNotReleased, dest, relErr)
	}
	return fmt.Errorf("dictation download: could not release the install lock on %s: %w", dest, relErr)
}

// isInstallDestName reports whether name is one path component naming an install
// destination. A destination is one path component: any other shape puts the
// lock file somewhere else, and two callers for the same destination would then
// lock different inodes and both proceed. Recovery runs the same check over the
// destination name it reads out of a marker, so a name off disk is never trusted
// further than one the caller supplied.
func isInstallDestName(name string) bool {
	return name != "" && name == filepath.Base(name) && name != "." && name != ".."
}

// lockDestination takes the Install lock for one destination under destRoot,
// waiting out another process that holds it until ctx ends or the budget runs
// out. The budget expiring is errInstallInProgress, which the caller answers by
// re-checking the destination rather than by failing.
func lockDestination(ctx context.Context, destRoot, dest string) (*destTxn, error) {
	deadline := time.Now().Add(installLockWait)
	for {
		txn, held, err := tryLockDestination(destRoot, dest)
		if err != nil {
			return nil, err
		}
		if !held {
			return txn, nil
		}
		// Waiting is the normal case: the holder is almost always another
		// process installing this very component, and failing on the first
		// contended try would turn that into a user-visible install failure.
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("%w: %s is locked by another process", errInstallInProgress, dest)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(installLockPoll):
		}
	}
}

// tryLockDestination takes one destination's Install lock without waiting. Held
// is not an error: a caller either waits the holder out, as lockDestination
// does, or leaves that destination alone, as the operator's reclaim does. An
// operator command cannot use the waiting form, because the wait budget covers a
// download and stalling a terminal for two minutes to report a refusal is not a
// refusal anyone reads.
func tryLockDestination(destRoot, dest string) (txn *destTxn, held bool, err error) {
	if !isInstallDestName(dest) {
		return nil, false, fmt.Errorf("dictation download: %q is not an install destination name", dest)
	}
	// destRoot is otherwise created lazily by the download this lock covers, and
	// lockutil opens the root with O_DIRECTORY, so on a first run there would be
	// no directory to take the lock in.
	if err := os.MkdirAll(destRoot, 0o755); err != nil {
		return nil, false, err
	}
	lockDir := filepath.Join(destRoot, installLockDir)
	if err := holderFS.mkdir(lockDir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, false, err
	}
	lock, err := acquireInstallLock(destRoot, filepath.Join(lockDir, dest+".lock"))
	if err != nil {
		if errors.Is(err, lockutil.ErrLockHeld) {
			return nil, true, nil
		}
		return nil, false, err
	}
	return &destTxn{root: destRoot, dest: dest, lock: lock}, false, nil
}

// usability answers whether a directory holds an install the caller can use.
// The error is the third answer a bool cannot carry: a probe that could not run
// says nothing about the copy, and reading it as "not usable" is what lets a
// transient fault license moving, parking, or deleting the last copy there is.
type usability func(dir string) (bool, error)

// check applies the predicate and settles the one error that is an answer: the
// expected install content being absent is what "not usable" means, and every
// other failure is the probe saying nothing at all. Callers stop the whole
// destination on the second, because a copy nobody could read may be the newest
// one there is.
func (p usability) check(dir string) (bool, error) {
	ok, err := p(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return ok, nil
}

// withDestinationLock runs fn under dest's Install lock. A wait that runs out is
// not by itself a failure: the likeliest reason is that the other process
// finished this exact install, so usable re-checks the destination before the
// caller is told anything is wrong.
func withDestinationLock(ctx context.Context, destRoot, dest string, usable usability, fn func(*destTxn) error) (err error) {
	txn, err := lockDestination(ctx, destRoot, dest)
	if err != nil {
		if errors.Is(err, errInstallInProgress) {
			// The recheck is the only evidence the wait was harmless. A probe
			// that could not run is not that evidence, so both facts are
			// reported rather than the timeout alone being answered with a
			// silent success.
			ok, probeErr := usable.check(filepath.Join(destRoot, dest))
			if probeErr != nil {
				return errors.Join(err, probeErr)
			}
			if ok {
				return nil
			}
		}
		return err
	}
	// fn returning nil means the destination is in the state the caller asked
	// for, which is exactly what a bare lock error would hide: the caller would
	// download the engine it just installed again.
	defer func() { err = mergeReleaseFailure(err, txn.release(), err == nil, dest) }()
	return fn(txn)
}

const holderSuffix = ".previous-"

// keptSuffix names a holder recovery decided to retain rather than restore or
// delete. It is a different prefix from holderSuffix so recovery's scan never
// enumerates a copy it has already ruled on, while the allocator still counts
// both and cannot reissue a number a Kept backup already carries.
const keptSuffix = ".kept-"

// holderSeqDigits is how many digits a sequenced name carries. Fixed width is
// what makes the grammar exact: a name one digit short is a name this code did
// not write, whatever else it looks like.
const holderSeqDigits = 20

// holderStamp reads back the ordering number in a holder name, reporting false
// for any name this code did not write. The grammar is exact and it is the
// first ownership filter: the install's own base name, then .previous- or
// .kept-, then twenty digits, then holderSeqSuffix, and nothing else. Loose
// parsing is what let a sibling merely NAMED like a holder be attributed to the
// install and moved on its account. No released version wrote a holder at all,
// so there is no older shape to accept.
func holderStamp(destDir, holder string) (int64, bool) {
	name := filepath.Base(holder)
	base := filepath.Base(destDir)
	var rest string
	switch {
	case strings.HasPrefix(name, base+holderSuffix):
		rest = name[len(base)+len(holderSuffix):]
	case strings.HasPrefix(name, base+keptSuffix):
		rest = name[len(base)+len(keptSuffix):]
	default:
		return 0, false
	}
	digits, ok := strings.CutSuffix(rest, holderSeqSuffix)
	if !ok || len(digits) != holderSeqDigits {
		return 0, false
	}
	for _, c := range []byte(digits) {
		// ParseInt would take a leading '+' or '-', and a signed number renders
		// back through %020d as a different string, so the writer and the reader
		// would disagree about a name they both accept.
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	stamp, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	return stamp, true
}

// holderSeqSuffix closes a sequenced holder name. It is what the grammar ends
// on, so a name that stops at the digits, or carries anything after the suffix,
// is not one this code wrote.
const holderSeqSuffix = "-seq"

// holderSeqAttempts bounds the walk up from a taken number, in the spirit of the
// retry limit os.MkdirTemp applies to its own random names.
const holderSeqAttempts = 10000

// holderMarkerFile names the transaction marker written inside every holder
// before anything is moved into it. The name alone is not ownership: any
// directory can be named to look like a holder, and moving or deleting one on
// its name is how a sibling that merely collides with the prefix loses its
// contents.
const holderMarkerFile = "txn"

// committedFile is the flag promoteStagedDir creates inside a holder after the
// publish rename succeeds. It is the only evidence that the copy in the holder
// was superseded by an install that actually landed, and so the only thing that
// licenses removing it.
const committedFile = "committed"

// holderMarkerKind is what this site writes in a marker's kind field. The two
// install sites use one marker format and different kinds, so a directory
// written by the other one is never read as this one's.
const holderMarkerKind = "dictation-promote"

// txnMarker is the ownership proof inside a holder: the transaction that created
// it, the destination it was created for, and the sequence in its name. All
// three have to agree with what recovery already knows before the holder is
// this code's to touch.
type txnMarker struct {
	Kind string `json:"kind"`
	Dest string `json:"dest"`
	Seq  int64  `json:"seq"`
}

// errMarkerMissing separates "there is no marker" from "the marker could not be
// read". The first is a directory this code cannot claim; the second is a
// filesystem fault, and treating them alike would let a transient read error
// silently reclassify a copy.
var errMarkerMissing = errors.New("no transaction marker")

// writeHolderMarker writes the marker atomically: a temp file in the same
// directory, then a rename. A torn write cannot then leave a partial marker that
// parses, which is what makes "the marker is missing" and "the marker is ours"
// the only two answers a crash can produce.
func writeHolderMarker(dir string, m txnMarker) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	f, err := holderFS.createTemp(dir, holderMarkerFile+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if err := writeAndSync(f, data); err != nil {
		_ = holderFS.removeAll(tmp)
		return err
	}
	if err := fsutil.RenameWithRetry(tmp, filepath.Join(dir, holderMarkerFile), holderFS.rename); err != nil {
		_ = holderFS.removeAll(tmp)
		return err
	}
	return nil
}

// writeAndSync fills the marker's temp file and gets it to disk before the
// rename that publishes it. Without the Sync the rename can be visible after a
// crash while the bytes behind it are not.
func writeAndSync(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// readHolderMarker reads a holder's marker. A missing one is errMarkerMissing;
// anything else is returned as it came back, so a caller can tell a directory it
// cannot claim from one it cannot read.
func readHolderMarker(dir string) (txnMarker, error) {
	data, err := holderFS.readFile(filepath.Join(dir, holderMarkerFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return txnMarker{}, fmt.Errorf("%w in %s", errMarkerMissing, dir)
		}
		return txnMarker{}, err
	}
	var m txnMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return txnMarker{}, fmt.Errorf("unreadable transaction marker in %s: %w", dir, err)
	}
	return m, nil
}

// writeCommitFlag records that the publish this holder's copy was set aside for
// actually landed. O_EXCL because a flag that is already there was written by a
// different transaction, and overwriting it would move the evidence.
func writeCommitFlag(dir string) error {
	f, err := holderFS.create(filepath.Join(dir, committedFile), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// holderCandidate is one holder recovery may act on, with the sequence its name
// and its marker agree on.
type holderCandidate struct {
	path string
	seq  int64
}

// ownedHoldersBeside splits the entries beside destDir into the holders this
// code can prove it wrote for THIS destination and the ones it cannot. Owned
// means the exact name grammar and a marker whose kind, destination, and
// sequence all agree with it; unowned means a name this code would have written
// with no such marker behind it, which is retained and reported, never moved.
// An entry that only collides with the prefix is neither: it was never this
// install's, so it is not even reported on its account.
func ownedHoldersBeside(destDir string) (owned []holderCandidate, unowned []string, unreadable string, err error) {
	// ReadDir and a prefix rather than filepath.Glob: destDir is a real path,
	// and a '[' anywhere in it opens a character class to Glob, which then
	// matches nothing and strands the install this exists to put back.
	parent := filepath.Dir(destDir)
	base := filepath.Base(destDir)
	entries, err := holderFS.readDir(parent)
	if err != nil {
		// A listing that failed is not a listing that found nothing. The two
		// are byte-identical to a caller reading only the slices, and reading
		// the second for the first is how an install still sitting in a holder
		// gets downloaded over.
		return nil, nil, "", err
	}
	prefix := base + holderSuffix
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || !strings.HasPrefix(name, prefix) {
			continue
		}
		path := filepath.Join(parent, name)
		seq, ok := holderStamp(destDir, name)
		if !ok {
			continue
		}
		m, err := readHolderMarker(path)
		if err != nil {
			if !errors.Is(err, errMarkerMissing) {
				// A marker this pass could not read is not a marker proving the
				// holder is someone else's. Walking past it would let recovery
				// install an older copy and let the next pass read that copy as
				// proof this one was superseded, which is the provenance loss
				// the no-fallback rule exists to prevent.
				return nil, nil, path, nil
			}
			unowned = append(unowned, path)
			continue
		}
		// Everything read off disk goes back through the check the write path
		// applied, and the whole marker has to agree: a marker naming another
		// destination or another sequence is evidence this directory belongs to
		// a transaction that is not the one being recovered.
		if m.Kind != holderMarkerKind || m.Seq != seq || m.Dest != base || !isInstallDestName(m.Dest) {
			continue
		}
		owned = append(owned, holderCandidate{path: path, seq: seq})
	}
	return owned, unowned, "", nil
}

// parkKeptHolder renames a holder under the Kept prefix, which is how a copy
// recovery will not restore and cannot prove superseded leaves the scan without
// leaving the disk. A plain rename: the Kept name is a distinct sequence nothing
// else claims, and a park that would have to clear something first is a park
// onto a copy that is not ours to remove.
func parkKeptHolder(holder string) error {
	kept, ok := keptHolderPath(holder)
	if !ok {
		return fmt.Errorf("%q is not a holder name", filepath.Base(holder))
	}
	return fsutil.RenameWithRetry(holder, kept, holderFS.rename)
}

// keptHolderPath is the Kept name a holder parks under: the same sequence, the
// other prefix. Recovery names it in its report before the rename as well as
// after, so the operator is told the same path whether the park lands or not.
func keptHolderPath(holder string) (string, bool) {
	dir, name := filepath.Split(holder)
	cut := strings.LastIndex(name, holderSuffix)
	if cut < 0 {
		return "", false
	}
	destDir := filepath.Join(dir, name[:cut])
	if _, ok := holderStamp(destDir, holder); !ok {
		return "", false
	}
	return filepath.Join(dir, name[:cut]+keptSuffix+name[cut+len(holderSuffix):]), true
}

// keptHolderStamp splits a Kept backup name into the install it was set aside
// for and its sequence. The install's own name is the leading part, so the
// grammar alone does NOT bound the name to one path component: "../x.kept-N-seq"
// parses as destination "../x". Callers check both halves are base names before
// joining either of them to anything.
func keptHolderStamp(name string) (base string, seq int64, ok bool) {
	cut := strings.LastIndex(name, keptSuffix)
	if cut <= 0 {
		return "", 0, false
	}
	base = name[:cut]
	seq, ok = holderStamp(base, name)
	if !ok {
		return "", 0, false
	}
	return base, seq, true
}

// KeptBackup is one copy recovery moved under the Kept prefix rather than
// deleted, as an operator sees it. Recovery never enumerates that prefix and
// nothing reclaims one on its own, so this listing is the only way a retained
// copy is found again. Owned false is a directory carrying the Kept name that
// nothing on disk attributes, listed beside the real backups so recovery's
// residue is visible rather than silent until a disk fills.
type KeptBackup struct {
	Path  string
	Dest  string
	Seq   int64
	Bytes int64
	Owned bool
}

// ListKeptBackups reports every Kept backup under an install root. The
// destination is reported only when the marker inside the copy agrees with the
// name it sits in: a name is what anything that can write in this directory
// says, and a copy attributed on its name alone is one an operator would remove
// believing it belonged to an install it never came from.
func ListKeptBackups(destRoot string) ([]KeptBackup, error) {
	entries, err := holderFS.readDir(destRoot)
	if err != nil {
		return nil, err
	}
	var out []KeptBackup
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		base, seq, ok := keptHolderStamp(entry.Name())
		if !ok {
			continue
		}
		path := filepath.Join(destRoot, entry.Name())
		backup := KeptBackup{Path: path, Seq: seq, Bytes: dirBytes(path)}
		if ownedKeptHolder(path, base, seq) == nil {
			backup.Dest = base
			backup.Owned = true
		}
		out = append(out, backup)
	}
	slices.SortFunc(out, func(a, b KeptBackup) int { return cmp.Compare(a.Path, b.Path) })
	return out, nil
}

// ownedKeptHolder is the whole ownership proof for a Kept backup: a marker this
// site wrote, for this destination, carrying the sequence its name carries. It
// is the same agreement ownedHoldersBeside requires before recovery touches a
// holder, applied here so an operator's delete is licensed by no less.
func ownedKeptHolder(path, base string, seq int64) error {
	m, err := readHolderMarker(path)
	if err != nil {
		return err
	}
	if m.Kind != holderMarkerKind || m.Seq != seq || m.Dest != base || !isInstallDestName(m.Dest) {
		return fmt.Errorf("the transaction marker in %s does not match its name", path)
	}
	return nil
}

// RemoveKeptBackup deletes the one Kept backup an operator named. It is not an
// rm: the name has to be a single component under destRoot, pass the Kept
// grammar, and carry a marker whose kind, destination, and sequence all agree,
// and the destination's Install lock has to be free. Anything else is refused
// and left for the operator to remove by hand, because a Kept backup here is
// often the only offline copy of an engine or a model.
func RemoveKeptBackup(destRoot, name string) (err error) {
	if !isInstallDestName(name) {
		// Ahead of every filesystem call: the Kept grammar accepts a leading
		// install name with a separator in it, so without this the join reaches
		// outside destRoot.
		return fmt.Errorf("dictation download: %q is not the name of an entry in %s", name, destRoot)
	}
	base, seq, ok := keptHolderStamp(name)
	if !ok || !isInstallDestName(base) {
		return fmt.Errorf("dictation download: %s is not a kept backup; recovery may still act on it, so remove it by hand", name)
	}
	path := filepath.Join(destRoot, name)
	if err := ownedKeptHolder(path, base, seq); err != nil {
		return fmt.Errorf("dictation download: nothing on disk attributes %s to an install (%w); remove it by hand", name, err)
	}
	txn, held, err := tryLockDestination(destRoot, base)
	if err != nil {
		return err
	}
	if held {
		return fmt.Errorf("dictation download: %s is being installed by another process; try again once it finishes", base)
	}
	// The removal is the mutation, and it has already happened by the time the
	// lock is released. Reported bare, a release failure reads as "nothing was
	// removed" and sends an operator back to a command whose work is done.
	defer func() { err = mergeReleaseFailure(err, txn.release(), err == nil, base) }()
	// Re-read under the lock. The check above ran with nothing excluding a live
	// promotion, and a directory that stopped being this install's in between is
	// one that must not be deleted on the strength of the earlier reading.
	if err := ownedKeptHolder(path, base, seq); err != nil {
		return fmt.Errorf("dictation download: %s is no longer attributable to %s (%w); leaving it in place", name, base, err)
	}
	return holderFS.removeAll(path)
}

// dirBytes sums the regular files under path. Unreadable entries are skipped
// rather than failing the listing: a size an operator cannot see is a worse
// answer than a size that is short, and the entry itself is still reported.
func dirBytes(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}

// nextHolderSeq is the number a new holder should claim: one past the highest
// already set aside for this install, counting BOTH the scanned prefix and the
// Kept one. Recovery parks a holder by renaming it under .kept-, and a sequence
// that stopped counting there would reissue a number a parked copy already
// carries. Holders for a different install are a separate sequence and are never
// compared against this one, and a name the grammar rejects raises nothing.
func nextHolderSeq(destDir string) (int64, error) {
	entries, err := holderFS.readDir(filepath.Dir(destDir))
	if err != nil {
		return 0, err
	}
	var high int64
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if stamp, ok := holderStamp(destDir, entry.Name()); ok && stamp > high {
			high = stamp
		}
	}
	if high == math.MaxInt64 {
		// The addition below would wrap negative, and %020d of a negative
		// renders a '-' the parser reads as empty digits, so the holder would
		// drop out of the ordering without saying so. Refuse instead.
		return 0, fmt.Errorf("a previous install of %s is named at the maximum sequence; remove it before installing again", filepath.Base(destDir))
	}
	return high + 1, nil
}

// createSequencedHolder claims the first free holder name from n upward. Nothing
// locks this directory, so exclusive creation is what arbitrates: two promotions
// racing cannot both win a name, and the loser walks up above the winner.
func createSequencedHolder(destDir string, n int64) (string, error) {
	if n < 1 {
		return "", fmt.Errorf("refusing to allocate holder sequence %d", n)
	}
	for i := 0; i < holderSeqAttempts; i++ {
		path := fmt.Sprintf("%s%s%020d%s", destDir, holderSuffix, n, holderSeqSuffix)
		// The writer and the reader agree on the format or nothing is written.
		if stamp, ok := holderStamp(destDir, path); !ok || stamp != n {
			return "", fmt.Errorf("holder name %q does not read back as sequence %d", filepath.Base(path), n)
		}
		err := holderFS.mkdir(path, 0o700)
		if err == nil {
			return path, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
		if n == math.MaxInt64 {
			return "", fmt.Errorf("holder sequence exhausted for %s", filepath.Base(destDir))
		}
		n++
	}
	return "", fmt.Errorf("could not claim a holder name for %s after %d attempts", filepath.Base(destDir), holderSeqAttempts)
}

// holderState is one owned holder after classification. Recovery decides what
// every candidate IS before it moves anything: a ruling made while the disk is
// half-read is a ruling made on a state that no longer exists by the time it is
// acted on, which is how a copy that was the last usable one gets deleted.
type holderState struct {
	holderCandidate
	// empty means the marker is there and the set-aside content is not, so the
	// holder provably holds no copy of any install.
	empty bool
	// committed means the publish this copy was set aside for actually landed.
	// It is the only evidence that licenses removing a copy.
	committed bool
	// usable is the CALLER's predicate applied to the copy itself. Recovery has
	// no opinion about what an install looks like; the consumer does.
	usable bool
}

// classifyHolder answers what one owned holder is, or reports that it cannot be
// read. Unusable and unreadable are deliberately different answers: the first is
// durable and the copy is skipped and kept, the second is a filesystem fault,
// and turning a fault into a classification is how a transient error becomes a
// permanent ruling about a copy nobody can get back.
func classifyHolder(c holderCandidate, published usability) (holderState, error) {
	st := holderState{holderCandidate: c}
	install := filepath.Join(c.path, "install")
	// ReadDir rather than Stat: a copy whose contents cannot be listed is one
	// whose usability cannot be decided either, and the predicate below would
	// answer "unusable" for it without ever having seen it.
	if _, err := holderFS.readDir(install); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return holderState{}, err
		}
		st.empty = true
		return st, nil
	}
	if _, err := holderFS.stat(filepath.Join(c.path, committedFile)); err == nil {
		st.committed = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return holderState{}, err
	}
	usable, err := published.check(install)
	if err != nil {
		return holderState{}, err
	}
	st.usable = usable
	return st, nil
}

// retainHolder gets one copy recovery will not restore out of the scan without
// getting it off the disk. An owned holder that provably holds nothing is
// removed; everything else moves under the Kept prefix, where the next pass does
// not look and the operator can still find it by name. A park that fails leaves
// the copy exactly where it is: the next pass reaches the same branch and tries
// again, and nothing here ever turns a retain into a delete.
func retainHolder(st holderState, report func(string)) {
	if st.empty {
		if err := holderFS.removeAll(st.path); err != nil {
			report(fmt.Sprintf("Could not remove the empty holder %s (%v)", st.path, err))
		}
		return
	}
	kept, ok := keptHolderPath(st.path)
	if !ok {
		report(fmt.Sprintf("Keeping a copy of this install in %s", st.path))
		return
	}
	if err := parkKeptHolder(st.path); err != nil {
		report(fmt.Sprintf("Keeping a copy of this install in %s; it could not be moved to %s (%v)", st.path, kept, err))
		return
	}
	report(fmt.Sprintf("Keeping a copy of this install in %s", kept))
}

// setAsideUnusableDest moves a destination that exists but holds no usable
// install into a fresh sequenced holder, so a usable copy can be restored over
// it. Fresh rather than the candidate's own holder: nesting one set-aside copy
// inside another is how a copy stops being findable by its own name. Only ever
// called once a candidate has been selected, so a destination nothing can
// replace is never taken apart.
func setAsideUnusableDest(destDir string) (string, error) {
	seq, err := nextHolderSeq(destDir)
	if err != nil {
		return "", err
	}
	holder, err := createSequencedHolder(destDir, seq)
	if err != nil {
		return "", err
	}
	claimed, ok := holderStamp(destDir, holder)
	if !ok {
		_ = holderFS.removeAll(holder)
		return "", fmt.Errorf("holder name %q is not one recovery can order", filepath.Base(holder))
	}
	// Same order the write path uses: the marker goes in before the first
	// destructive rename, so a stop in this window leaves a holder recovery can
	// prove is its own rather than one nothing on disk claims.
	if err := writeHolderMarker(holder, txnMarker{Kind: holderMarkerKind, Dest: filepath.Base(destDir), Seq: claimed}); err != nil {
		_ = holderFS.removeAll(holder)
		return "", err
	}
	if err := fsutil.RenameWithRetry(destDir, filepath.Join(holder, "install"), holderFS.rename); err != nil {
		// Owned and holding nothing, which is the one shape that costs nothing
		// to remove. Leaving it would accumulate scratch on every failed pass.
		_ = holderFS.removeAll(holder)
		return "", err
	}
	return holder, nil
}

// restoreInterruptedPromotion reconciles one destination with every copy of it
// beside it. A process stopped between promoteStagedDir's two renames leaves the
// only usable install in a .previous-* holder nothing else looks at, and an
// offline caller has no download to fall back on; a failed cleanup leaves a copy
// that IS superseded; and recovery itself can leave a destination holding a tree
// the caller cannot use. So every candidate is classified before anything moves,
// and the ruling is a function of on-disk state alone: a second pass with no
// memory of the first reaches the same answer.
//
// published reports whether a directory holds an install this caller can
// actually use. It is the whole basis of every decision here, because "there is
// something at destDir" and "a promotion published there" are different claims,
// and only the second makes a copy beside it superseded.
func restoreInterruptedPromotion(txn *destTxn, destDir string, published usability, report func(string)) error {
	if report == nil {
		report = func(string) {}
	}
	// Recovery moves and deletes whole installs. Without destDir's Install lock
	// it can restore a holder a promotion in another process is parked on and
	// remove it, leaving that promotion's rollback nothing to put back.
	if !txn.holds(destDir) {
		report(fmt.Sprintf("Skipping recovery of %s: no install lock is held for it", filepath.Base(destDir)))
		return nil
	}
	// Every ruling below rests on the predicate. With none there is no evidence
	// for any of them, and acting anyway would delete or publish a copy on the
	// strength of a directory merely existing.
	if published == nil {
		report(fmt.Sprintf("Skipping recovery of %s: no usability check was supplied", filepath.Base(destDir)))
		return nil
	}
	// Only holders this code can prove it wrote for THIS destination. A sibling
	// that merely collides with the prefix is not a copy of this install and is
	// never restored from, removed, or reported on its account.
	owned, unowned, unreadable, err := ownedHoldersBeside(destDir)
	if err != nil {
		report(fmt.Sprintf("Stopping recovery of %s: the copies beside it could not be listed (%v)", filepath.Base(destDir), err))
		return err
	}
	if unreadable != "" {
		// Same rule as a listing that failed: with no ruling on the copy beside
		// the destination there is no evidence for any decision here. Returning
		// nil hands the caller a clean run, and the caller answers it by
		// downloading over the very transaction whose state nobody could read.
		report(fmt.Sprintf("dictation: leaving every retained copy for %s in place until %s can be read", filepath.Base(destDir), unreadable))
		return fmt.Errorf("dictation download: stopping recovery of %s: the retained copy in %s could not be read", filepath.Base(destDir), unreadable)
	}
	for _, path := range unowned {
		report(fmt.Sprintf("Keeping %s: nothing in it identifies it as a copy of %s", path, filepath.Base(destDir)))
	}
	states := make([]holderState, 0, len(owned))
	for _, candidate := range owned {
		st, err := classifyHolder(candidate, published)
		if err != nil {
			// One unreadable candidate stops the whole destination. Ruling on
			// the rest would mean choosing between copies while one of them is
			// unread, and the copy nobody could read may be the newest.
			report(fmt.Sprintf("Stopping recovery of %s: %s could not be read (%v)", filepath.Base(destDir), candidate.path, err))
			return err
		}
		states = append(states, st)
	}
	// Newest first: the sequence in the name, which the marker agrees with, is
	// the only record of which copy was live last. No decision here reads a
	// clock, so a backward clock cannot reorder them.
	slices.SortStableFunc(states, func(a, b holderState) int {
		return cmp.Compare(b.seq, a.seq)
	})

	destPresent := false
	if _, err := holderFS.lstat(destDir); err == nil {
		destPresent = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		report(fmt.Sprintf("Stopping recovery of %s: it could not be read (%v)", filepath.Base(destDir), err))
		return err
	}

	destUsable := false
	if destPresent {
		usable, err := published.check(destDir)
		if err != nil {
			// Same rule as an unreadable candidate: without an answer about the
			// destination there is no telling whether a copy beside it is
			// superseded or the last one left, and every branch below turns on
			// exactly that.
			report(fmt.Sprintf("Stopping recovery of %s: it could not be read (%v)", filepath.Base(destDir), err))
			return err
		}
		destUsable = usable
	}
	if destUsable {
		// The destination holds a real install, so every copy beside it was set
		// aside by some earlier transaction. Only the ones carrying the commit
		// flag are provably superseded by it; the rest may still be the last
		// usable copy of something and are kept.
		for _, st := range states {
			if st.committed && !st.empty {
				if err := holderFS.removeAll(st.path); err != nil {
					report(fmt.Sprintf("Could not remove the superseded copy in %s (%v)", st.path, err))
				}
				continue
			}
			retainHolder(st, report)
		}
		return nil
	}

	// The destination is absent or holds nothing this caller can use, so a
	// usable copy beside it is what should be live. Uncommitted first: it was
	// set aside by the transaction that never finished, which makes it the most
	// recent state anything can prove.
	selected := -1
	for i, st := range states {
		if !st.empty && !st.committed && st.usable {
			selected = i
			break
		}
	}
	if selected < 0 {
		for i, st := range states {
			if !st.empty && st.committed && st.usable {
				selected = i
				break
			}
		}
	}
	if selected < 0 {
		if destPresent {
			// Nothing beside it can replace it, so taking it apart would leave
			// the caller with no destination at all instead of an unusable one.
			report(fmt.Sprintf("%s holds no usable install and no copy beside it can replace it; it is left as it is", destDir))
		}
		for _, st := range states {
			retainHolder(st, report)
		}
		return nil
	}

	// Classify, select, THEN set aside. A husk moved before a candidate is
	// chosen is a destination taken apart for a restore that may never happen,
	// and a failed restore would strand it under a prefix recovery never reads.
	husk := ""
	if destPresent {
		var err error
		husk, err = setAsideUnusableDest(destDir)
		if err != nil {
			report(fmt.Sprintf("Leaving %s as it is: it could not be set aside (%v)", destDir, err))
			return nil
		}
	}
	winner := states[selected]
	if err := fsutil.RenameWithRetry(filepath.Join(winner.path, "install"), destDir, holderFS.rename); err != nil {
		// No fallback to an older copy: installing one would put a tree at the
		// destination that the next pass reads as evidence the newer copy was
		// superseded, which is exactly how provenance is lost.
		if husk != "" {
			if backErr := fsutil.RenameWithRetry(filepath.Join(husk, "install"), destDir, holderFS.rename); backErr != nil {
				report(fmt.Sprintf("%s could not be put back and is kept in %s (%v)", destDir, husk, backErr))
			} else if rmErr := holderFS.removeAll(husk); rmErr != nil {
				report(fmt.Sprintf("Could not remove the empty holder %s (%v)", husk, rmErr))
			}
		}
		report(fmt.Sprintf("Keeping the copy of %s in %s: it could not be moved back into place (%v)", filepath.Base(destDir), winner.path, err))
		return nil
	}
	// The winner's holder is owned and now holds nothing.
	if err := holderFS.removeAll(winner.path); err != nil {
		report(fmt.Sprintf("Could not remove the empty holder %s (%v)", winner.path, err))
	}
	if husk != "" {
		retainHolder(holderState{holderCandidate: holderCandidate{path: husk}}, report)
	}
	for i, st := range states {
		if i != selected {
			retainHolder(st, report)
		}
	}
	return nil
}

// promoteStagedDir moves stageDir into place at destDir. os.Rename refuses to
// overwrite a non-empty directory, so any previous install has to move out of
// the way first; it is set aside rather than deleted, and put back if the
// promotion fails, so destDir is never left holding no install at all. If the
// restore fails too, the set-aside copy is kept and named in the error.
func promoteStagedDir(txn *destTxn, stageDir, destDir, label string, report func(string)) error {
	// Normalized once so every later report is one call rather than a nil check
	// a new one can forget. A caller with nowhere to report to still gets the
	// same behavior; only the message goes nowhere.
	if report == nil {
		report = func(string) {}
	}
	// Same reason recovery refuses: the window between the two renames has the
	// only copy of the install in a holder, and nothing else keeps a concurrent
	// recovery out of it.
	if !txn.holds(destDir) {
		{
			report(fmt.Sprintf("Refusing to install %s: no install lock is held for %s", label, filepath.Base(destDir)))
		}
		return fmt.Errorf("promoting staged %s: no install lock is held for %s", label, destDir)
	}
	holder := ""
	cleanupHolder := true
	defer func() {
		if holder != "" && cleanupHolder {
			// A failure here strands a whole copy of the previous install under
			// a name recovery no longer enumerates, so say so rather than
			// leaking it silently: the operator's reclaim command is the only
			// thing that removes it afterwards.
			if err := holderFS.removeAll(holder); err != nil {
				report(fmt.Sprintf("dictation: could not remove the superseded %s copy in %s: %v", label, holder, err))
			}
		}
	}()

	restore := func() error { return nil }
	if _, err := holderFS.lstat(destDir); err == nil {
		var seq int64
		seq, err = nextHolderSeq(destDir)
		if err != nil {
			return fmt.Errorf("setting aside previous %s install: %w", label, err)
		}
		holder, err = createSequencedHolder(destDir, seq)
		if err != nil {
			return fmt.Errorf("setting aside previous %s install: %w", label, err)
		}
		// createSequencedHolder walks up from seq when a name is taken, so the
		// marker has to carry the number actually claimed. A marker that
		// disagrees with the name it sits in is not ownership, and recovery
		// would leave the copy in place forever.
		claimed, ok := holderStamp(destDir, holder)
		if !ok {
			return fmt.Errorf("setting aside previous %s install: holder name %q is not one recovery can order", label, filepath.Base(holder))
		}
		// The marker goes in BEFORE the first destructive rename. A holder that
		// takes the only copy of an install before it can be proven ours is one
		// nothing on disk lets recovery claim.
		if err := writeHolderMarker(holder, txnMarker{Kind: holderMarkerKind, Dest: filepath.Base(destDir), Seq: claimed}); err != nil {
			return fmt.Errorf("setting aside previous %s install: %w", label, err)
		}
		previous := filepath.Join(holder, "install")
		if err := fsutil.RenameWithRetry(destDir, previous, holderFS.rename); err != nil {
			return fmt.Errorf("setting aside previous %s install: %w", label, err)
		}
		restore = func() error { return fsutil.RenameWithRetry(previous, destDir, holderFS.rename) }
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("checking previous %s install: %w", label, err)
	}

	if err := fsutil.RenameWithRetry(stageDir, destDir, holderFS.rename); err != nil {
		if restoreErr := restore(); restoreErr != nil {
			cleanupHolder = false
			return fmt.Errorf("promoting staged %s: %w (previous install left in %s: %v)", label, err, holder, restoreErr)
		}
		return fmt.Errorf("promoting staged %s: %w", label, err)
	}
	if holder != "" {
		// The flag follows the publish and precedes the cleanup, so the only
		// thing a crash in that window costs is a retained copy. Writing it any
		// earlier would license deleting a copy that is still the only one.
		if err := writeCommitFlag(holder); err != nil {
			// The install itself landed. Deleting a whole previous install on
			// the word of a step that just failed is the one outcome worse than
			// leaving it on disk, so the copy stays and the report says where.
			cleanupHolder = false
			if report != nil {
				report(fmt.Sprintf("Installed %s, but the previous install could not be marked superseded and is kept in %s (%v)", label, holder, err))
			}
		}
	}
	return nil
}

// extractTarBz2 unpacks a bzip2-compressed tar into destDir, guarding against
// path-traversal entries. It reports extraction progress by how much of the
// (compressed) archive it has consumed — the same MB scale as the download, so
// "Extracting … 50% · 520/1041 MB" lines up with the download that preceded it.
func extractTarBz2(archivePath, destDir, label string, progress func(string)) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	var src io.Reader = f
	if progress != nil {
		if info, statErr := f.Stat(); statErr == nil && info.Size() > 0 {
			src = &progressReader{r: f, total: info.Size(), label: label, report: progress, lastPct: -1}
		}
	}
	tr := tar.NewReader(bzip2.NewReader(src))
	destAbs, err := filepath.Abs(destDir)
	if err != nil {
		return err
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(destDir, hdr.Name)
		absTarget, err := filepath.Abs(target)
		if err != nil {
			return err
		}
		if absTarget != destAbs && !strings.HasPrefix(absTarget, destAbs+string(os.PathSeparator)) {
			return fmt.Errorf("dictation download: archive entry %q escapes destination", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		}
	}
}

// flattenSingleChild returns the sole subdirectory of dir (the tarball's
// top-level folder) when dir contains exactly one directory entry.
func flattenSingleChild(dir string) (string, error) {
	entries, err := holderFS.readDir(dir)
	if err != nil {
		return "", err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) == 1 {
		return filepath.Join(dir, dirs[0]), nil
	}
	return dir, nil
}

// ModelDownloaded reports whether a model variant is already extracted under
// destRoot (so the picker can mark it as installed). dirName is the variant's
// DirName.
func ModelDownloaded(destRoot, dirName string) bool {
	if destRoot == "" || dirName == "" {
		return false
	}
	// Deliberately boolean while the transaction's own predicate is not: the
	// picker draws a badge, and a probe that could not run is best drawn as
	// "not downloaded". Recovery makes the opposite call, because there it
	// licenses moving and deleting copies.
	present, _ := dirHasModel(filepath.Join(destRoot, dirName))
	return present
}

// EngineDownloaded reports whether the shared sherpa-onnx engine is already on
// disk for this platform (one engine serves every model), so the picker can drop
// the engine's size from a model's download total once it's been fetched.
func EngineDownloaded(destRoot, version string) bool {
	if destRoot == "" {
		return false
	}
	if version == "" {
		version = DefaultSherpaVersion
	}
	key := platformKey()
	// Both layouts, for the same reason EnsureLocalEngine reads both: a tag that
	// is not one path component extracted into a nested directory before the
	// destination name became a lock key, and a badge that missed that copy
	// would offer a download of an engine already on the disk. The raw-tag path
	// goes through the same containment check the install does — the tag is a
	// config value, and joining it unvetted would let a badge probe a path
	// outside the install root.
	dirs := []string{filepath.Join(destRoot, engineDestName(version, key))}
	if rawDir, ok := compatEngineDir(destRoot, "engine-"+version+"-"+key); ok && rawDir != dirs[0] {
		dirs = append(dirs, rawDir)
	}
	for _, dir := range dirs {
		bin, _ := resolveEnginePaths(dir, runtime.GOOS == "windows")
		// Boolean for the same reason ModelDownloaded is: a badge, not a verdict.
		if present, _ := fileExistsErr(bin); present {
			return true
		}
	}
	return false
}

func dirHasModel(dir string) (bool, error) {
	present, err := hasTokensFile(dir)
	if present || err != nil {
		return present, err
	}
	child, err := flattenSingleChild(dir)
	if err != nil {
		return false, err
	}
	if child == dir {
		return false, nil
	}
	return hasTokensFile(child)
}

// hasTokensFile reports whether dir holds a sherpa tokens file — usually
// "tokens.txt", but some families (Whisper) prefix it as "<model>-tokens.txt".
func hasTokensFile(dir string) (bool, error) {
	entries, err := holderFS.readDir(dir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), "tokens.txt") {
			return true, nil
		}
	}
	return false, nil
}

// fileExistsErr is the error-aware form: a probe that could not run is not a
// probe that found nothing.
func fileExistsErr(path string) (bool, error) {
	info, err := holderFS.stat(path)
	if err != nil {
		return false, err
	}
	return !info.IsDir(), nil
}

func fileExists(path string) bool {
	present, _ := fileExistsErr(path)
	return present
}
