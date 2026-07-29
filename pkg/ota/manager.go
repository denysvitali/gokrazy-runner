// Package ota implements over-the-air updates for the gokrazy-runner
// appliance. It downloads a release asset from GitHub and streams it into
// the gokrazy updater (/update/root), then triggers a partition switch and
// reboot.
package ota

import (
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gokrazy/updater"
)

const (
	DefaultOwner         = "denysvitali"
	DefaultRepo          = "gokrazy-runner"
	DefaultAssetName     = "gokrazy-runner-rpi4b-root.squashfs.gz"
	DefaultBootAssetName = "gokrazy-runner-rpi4b-boot.fat.gz"
	DefaultGitHubAPIURL  = "https://api.github.com"
	// UpdateInsecureEnv enables TLS verification bypass for self-signed
	// gokrazy updater endpoints.
	UpdateInsecureEnv = "OTA_GOKRAZY_INSECURE"

	DefaultHistoryPath = "/perm/ota-install-history.json"

	maxInstallHistoryEntries = 20
)

// PasswordFunc returns the current gokrazy update password used to
// authenticate against the loopback /update/ endpoint.
type PasswordFunc func() string

type Installer interface {
	Install(ctx context.Context, images Images, progress InstallProgressFunc) error
}

type InstallProgressFunc func(InstallProgress)

type InstallProgress struct {
	Phase           string
	Message         string
	ProgressPercent float64
}

// GokrazyInstaller streams a root filesystem image into gokrazy's local
// /update/root endpoint, then switches partitions and reboots.
type GokrazyInstaller struct {
	BaseURL            string
	HTTPClient         *http.Client
	InsecureSkipVerify bool
}

// Images are the partition images to install.
//
// Boot is optional. It matters because the kernel and device trees live in
// the boot partition, not the root filesystem: an update that streams only
// the root leaves the device running its old kernel, which is how a build
// that shipped /lib/modules/6.18.34-v8 ended up on a device running
// 6.12.47-v8 and finding no modules for itself.
type Images struct {
	Root io.Reader
	// Boot is opened only after the root stream succeeded, so a failure
	// mid-download never leaves a new kernel paired with an old userspace.
	// Nil for releases that publish no boot image, and for the URL and
	// upload install paths.
	Boot func(ctx context.Context) (io.ReadCloser, error)
}

func (i GokrazyInstaller) Install(ctx context.Context, images Images, progress InstallProgressFunc) error {
	baseURL := normalizeUpdateBaseURL(i.BaseURL)
	if baseURL == "" {
		return errors.New("ota: empty gokrazy updater base URL")
	}

	client := i.httpClient(baseURL)

	target, err := NewUpdateTarget(ctx, baseURL, client)
	if err != nil {
		return fmt.Errorf("connect to gokrazy updater: %w", err)
	}
	// Root first: it goes to the inactive partition, so it cannot break the
	// running system if the transfer fails.
	reportInstallProgress(progress, "flashing", "Downloading and flashing OTA image", 10)
	if err := target.StreamTo(ctx, "root", images.Root); err != nil {
		return fmt.Errorf("stream root image: %w", err)
	}

	if images.Boot != nil {
		reportInstallProgress(progress, "flashing-boot", "Updating boot partition (kernel)", 80)
		boot, err := images.Boot(ctx)
		if err != nil {
			return fmt.Errorf("fetch boot image: %w", err)
		}
		defer boot.Close()
		// The boot partition is not A/B: this overwrites the kernel the
		// device is currently running from, which is also what `gok update`
		// does. Rolling back means installing an older release.
		if err := target.StreamTo(ctx, "boot", boot); err != nil {
			return fmt.Errorf("stream boot image: %w", err)
		}
	}

	reportInstallProgress(progress, "switching", "Switching root partition", 90)
	if err := target.Switch(ctx); err != nil {
		return fmt.Errorf("switch root partition: %w", err)
	}
	reportInstallProgress(progress, "rebooting", "Requesting reboot", 95)
	if err := target.Reboot(ctx); err != nil {
		return fmt.Errorf("reboot: %w", err)
	}
	return nil
}

func (i GokrazyInstaller) httpClient(baseURL string) *http.Client {
	client := i.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Minute}
	}
	return configureUpdateHTTPClient(client, baseURL, i.InsecureSkipVerify)
}

// Options configures a Manager.
type Options struct {
	Owner         string
	Repo          string
	AssetName     string
	BootAssetName string
	APIURL        string
	HistoryPath   string
	// TokenPath is the file holding an optional GitHub API token.
	// Defaults to DefaultTokenPath.
	TokenPath string
	// Password returns the current gokrazy update password (used to build
	// http://gokrazy:<pw>@127.0.0.1/). Required unless Installer is set.
	Password PasswordFunc
	// Installer overrides the default GokrazyInstaller; mainly for tests.
	Installer Installer
	// HTTPClient overrides the default GitHub HTTP client.
	HTTPClient *http.Client
}

type Manager struct {
	owner         string
	repo          string
	assetName     string
	bootAssetName string
	apiURL        string
	historyPath   string
	tokenPath     string

	httpClient *http.Client
	password   PasswordFunc
	installer  Installer
	insecure   bool

	mu             sync.Mutex
	status         Status
	installHistory []InstallHistoryEntry
	cache          releaseCache
}

type Status struct {
	State            string    `json:"state"`
	Phase            string    `json:"phase,omitempty"`
	Message          string    `json:"message,omitempty"`
	Release          string    `json:"release,omitempty"`
	Asset            string    `json:"asset,omitempty"`
	AssetURL         string    `json:"asset_url,omitempty"`
	PublishedAt      time.Time `json:"published_at,omitempty"`
	StartedAt        time.Time `json:"started_at,omitempty"`
	FinishedAt       time.Time `json:"finished_at,omitempty"`
	ProgressPercent  float64   `json:"progress_percent,omitempty"`
	DownloadedBytes  int64     `json:"downloaded_bytes,omitempty"`
	TotalBytes       int64     `json:"total_bytes,omitempty"`
	DownloadSpeedBps float64   `json:"download_speed_bps,omitempty"`
	Error            string    `json:"error,omitempty"`
}

type InstallHistoryEntry struct {
	Release    string    `json:"release"`
	Asset      string    `json:"asset"`
	AssetURL   string    `json:"asset_url"`
	State      string    `json:"state"`
	Message    string    `json:"message,omitempty"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

type Release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []Asset   `json:"assets"`
	HTMLURL     string    `json:"html_url"`
}

type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// NewManager builds a Manager from Options. Defaults are filled in for any
// zero-value fields. Either Options.Password or Options.Installer must be
// non-nil.
func NewManager(opts Options) (*Manager, error) {
	owner := valueDefault(opts.Owner, envDefault("OTA_GITHUB_OWNER", DefaultOwner))
	repo := valueDefault(opts.Repo, envDefault("OTA_GITHUB_REPO", DefaultRepo))
	asset := valueDefault(opts.AssetName, envDefault("OTA_RELEASE_ASSET", DefaultAssetName))
	bootAsset := valueDefault(opts.BootAssetName, envDefault("OTA_BOOT_ASSET", DefaultBootAssetName))
	api := valueDefault(opts.APIURL, envDefault("OTA_GITHUB_API_URL", DefaultGitHubAPIURL))
	histPath := valueDefault(opts.HistoryPath, DefaultHistoryPath)
	tokenPath := valueDefault(opts.TokenPath, envDefault("OTA_GITHUB_TOKEN_PATH", DefaultTokenPath))

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Minute}
	}

	insecure := envBool(UpdateInsecureEnv, false)

	mgr := &Manager{
		owner:          owner,
		repo:           repo,
		assetName:      asset,
		bootAssetName:  bootAsset,
		apiURL:         api,
		historyPath:    histPath,
		tokenPath:      tokenPath,
		httpClient:     httpClient,
		password:       opts.Password,
		installer:      opts.Installer,
		insecure:       insecure,
		status:         Status{State: "idle"},
		installHistory: []InstallHistoryEntry{},
	}
	if mgr.installer == nil && mgr.password == nil {
		return nil, errors.New("ota: Options.Password or Options.Installer is required")
	}
	_ = mgr.loadInstallHistory()
	return mgr, nil
}

func (m *Manager) Owner() string         { return m.owner }
func (m *Manager) Repo() string          { return m.repo }
func (m *Manager) AssetName() string     { return m.assetName }
func (m *Manager) BootAssetName() string { return m.bootAssetName }

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

func (m *Manager) InstallationHistory() []InstallHistoryEntry {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]InstallHistoryEntry, len(m.installHistory))
	copy(out, m.installHistory)
	return out
}

func (m *Manager) loadInstallHistory() error {
	// #nosec G304 -- historyPath is configured via trusted Options.
	data, err := os.ReadFile(m.historyPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var history []InstallHistoryEntry
	if err := json.Unmarshal(data, &history); err != nil {
		return err
	}
	if len(history) > maxInstallHistoryEntries {
		history = history[len(history)-maxInstallHistoryEntries:]
	}
	m.mu.Lock()
	m.installHistory = history
	m.mu.Unlock()
	return nil
}

func (m *Manager) recordInstallHistory(entry InstallHistoryEntry) {
	m.mu.Lock()
	m.installHistory = append(m.installHistory, entry)
	if len(m.installHistory) > maxInstallHistoryEntries {
		m.installHistory = m.installHistory[len(m.installHistory)-maxInstallHistoryEntries:]
	}
	history := make([]InstallHistoryEntry, len(m.installHistory))
	copy(history, m.installHistory)
	m.mu.Unlock()

	_ = saveJSONFile(m.historyPath, history)
}

func saveJSONFile(path string, v any) error {
	if path == "" {
		return nil
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil { // #nosec G306 -- non-secret state file
		return err
	}
	return os.Rename(tmp, path)
}

func (m *Manager) historyFromStatus(status Status) InstallHistoryEntry {
	return InstallHistoryEntry{
		Release:    status.Release,
		Asset:      status.Asset,
		AssetURL:   status.AssetURL,
		State:      status.State,
		Message:    status.Message,
		Error:      status.Error,
		StartedAt:  status.StartedAt,
		FinishedAt: status.FinishedAt,
	}
}

func (m *Manager) Start(ctx context.Context) (Status, error) {
	return m.StartWithRelease(ctx, "")
}

func (m *Manager) StartWithRelease(ctx context.Context, release string) (Status, error) {
	release = strings.TrimSpace(release)

	m.mu.Lock()
	if m.status.State == "checking" || m.status.State == "downloading" || m.status.State == "installing" {
		status := m.status
		m.mu.Unlock()
		return status, errors.New("OTA installation is already running")
	}
	m.status = Status{
		State:           "checking",
		Phase:           "checking",
		Message:         "Checking GitHub releases",
		StartedAt:       time.Now(),
		ProgressPercent: 2,
	}
	status := m.status
	m.mu.Unlock()

	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Hour)
	go func() {
		defer cancel()
		m.run(runCtx, release)
	}()
	return status, nil
}

func (m *Manager) LatestRelease(ctx context.Context) (*Release, *Asset, error) {
	releases, err := m.AvailableReleases(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(releases) == 0 {
		return nil, nil, errors.New("no GitHub releases found")
	}
	return &releases[0], &releases[0].Assets[0], nil
}

func (m *Manager) SelectRelease(ctx context.Context, tag string) (*Release, *Asset, error) {
	if strings.EqualFold(strings.TrimSpace(tag), "latest") || strings.TrimSpace(tag) == "" {
		return m.LatestRelease(ctx)
	}
	releases, err := m.AvailableReleases(ctx)
	if err != nil {
		return nil, nil, err
	}
	for i := range releases {
		if releases[i].TagName == tag {
			return &releases[i], &releases[i].Assets[0], nil
		}
	}
	return nil, nil, fmt.Errorf("release %q not found", tag)
}

func (m *Manager) run(ctx context.Context, releaseTag string) {
	release, asset, err := m.SelectRelease(ctx, releaseTag)
	if err != nil {
		m.fail(err)
		return
	}

	m.set(Status{
		State:           "downloading",
		Phase:           "downloading",
		Message:         "Preparing OTA download",
		Release:         release.TagName,
		Asset:           asset.Name,
		AssetURL:        asset.BrowserDownloadURL,
		PublishedAt:     release.PublishedAt,
		StartedAt:       m.Status().StartedAt,
		ProgressPercent: 5,
		TotalBytes:      asset.Size,
	})

	m.downloadAndInstall(ctx, asset.BrowserDownloadURL, asset.Size, m.bootFetcher(release))
}

// bootFetcher returns a function that downloads the release's boot image, or
// nil when the release does not publish one — releases built before boot
// images were added are still installable, they just leave the kernel alone.
func (m *Manager) bootFetcher(release *Release) func(context.Context) (io.ReadCloser, error) {
	if release == nil || m.bootAssetName == "" {
		return nil
	}
	asset := findAsset(release.Assets, m.bootAssetName)
	if asset == nil {
		log.Printf("ota: release %s publishes no %s; leaving the boot partition (kernel) untouched",
			release.TagName, m.bootAssetName)
		return nil
	}
	url := asset.BrowserDownloadURL
	return func(ctx context.Context) (io.ReadCloser, error) {
		body, err := m.openDownload(ctx, url)
		if err != nil {
			return nil, err
		}
		gz, err := gzip.NewReader(body)
		if err != nil {
			body.Close()
			return nil, fmt.Errorf("open gzip boot image: %w", err)
		}
		return gzipReadCloser{gz: gz, under: body}, nil
	}
}

// gzipReadCloser closes both the gzip reader and the response body it wraps.
type gzipReadCloser struct {
	gz    *gzip.Reader
	under io.ReadCloser
}

func (g gzipReadCloser) Read(p []byte) (int, error) { return g.gz.Read(p) }

func (g gzipReadCloser) Close() error {
	err := g.gz.Close()
	if cerr := g.under.Close(); err == nil {
		err = cerr
	}
	return err
}

// downloadAndInstall fetches downloadURL and streams it into the updater.
// The caller is responsible for having set the "downloading" status first.
func (m *Manager) downloadAndInstall(ctx context.Context, downloadURL string, expectedSize int64, boot func(context.Context) (io.ReadCloser, error)) {
	body, err := m.openDownload(ctx, downloadURL)
	if err != nil {
		m.fail(err)
		return
	}
	defer body.Close()

	totalBytes := expectedSize
	if totalBytes <= 0 {
		if sized, ok := body.(interface{ Size() int64 }); ok {
			totalBytes = sized.Size()
		}
	}
	m.installStream(ctx, body, totalBytes, boot)
}

// openDownload issues the GET and returns the body, attaching the GitHub
// token only for GitHub hosts so a redirect elsewhere never leaks it.
func (m *Manager) openDownload(ctx context.Context, downloadURL string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, err
	}
	if token := m.githubToken(); token != "" && isGitHubHost(downloadURL) {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download OTA asset: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("download OTA asset: %s returned %s", downloadURL, resp.Status)
	}
	return sizedReadCloser{ReadCloser: resp.Body, size: resp.ContentLength}, nil
}

type sizedReadCloser struct {
	io.ReadCloser
	size int64
}

func (s sizedReadCloser) Size() int64 { return s.size }

// installStream gunzips r and streams it into the gokrazy updater.
func (m *Manager) installStream(ctx context.Context, r io.Reader, totalBytes int64, boot func(context.Context) (io.ReadCloser, error)) {
	body := newDownloadProgressReader(r, totalBytes, m.updateDownloadProgress)

	gz, err := gzip.NewReader(body)
	if err != nil {
		m.fail(fmt.Errorf("open gzip OTA asset: %w", err))
		return
	}
	defer gz.Close()

	m.updateInstallProgress(InstallProgress{
		Phase:           "flashing",
		Message:         "Downloading and flashing OTA image",
		ProgressPercent: 10,
	})
	if err := m.resolveInstaller().Install(ctx, Images{Root: gz, Boot: boot}, m.updateInstallProgress); err != nil {
		m.fail(err)
		return
	}

	status := m.Status()
	status.State = "installed"
	status.Phase = "installed"
	status.Message = "OTA image installed; reboot requested"
	status.FinishedAt = time.Now()
	status.ProgressPercent = 100
	m.set(status)
	m.recordInstallHistory(m.historyFromStatus(status))
}

func (m *Manager) AvailableReleases(ctx context.Context) ([]Release, error) {
	releases, err := m.fetchReleases(ctx)
	if err != nil {
		return nil, err
	}

	sort.SliceStable(releases, func(i, j int) bool {
		return releases[i].PublishedAt.After(releases[j].PublishedAt)
	})

	filtered := make([]Release, 0, len(releases))
	for i := range releases {
		release := releases[i]
		if release.Draft {
			continue
		}
		if asset := findAsset(release.Assets, m.assetName); asset != nil {
			// Narrow to the assets we install. The root image stays first:
			// callers (and the web UI's size display) treat it as the
			// release's headline asset. The boot image has to survive this
			// filter or bootFetcher can never find it and the kernel is
			// silently never updated.
			kept := []Asset{*asset}
			if boot := findAsset(release.Assets, m.bootAssetName); boot != nil {
				kept = append(kept, *boot)
			}
			release.Assets = kept
			filtered = append(filtered, release)
		}
	}

	if len(filtered) == 0 {
		return nil, errors.New("no release contains " + m.assetName)
	}
	return filtered, nil
}

// fetchReleases lists releases, reusing the cached listing for
// releaseCacheTTL and revalidating with an ETag afterwards. GitHub does not
// charge 304 responses against the rate limit, and a stale cache is still
// served when the API errors out (rate limiting included) so the UI keeps
// working.
func (m *Manager) fetchReleases(ctx context.Context) ([]Release, error) {
	m.mu.Lock()
	cached := m.cache
	m.mu.Unlock()

	if len(cached.releases) > 0 && time.Since(cached.fetchedAt) < releaseCacheTTL {
		return cached.releases, nil
	}

	apiURL := strings.TrimRight(m.apiURL, "/")
	reqURL := fmt.Sprintf("%s/repos/%s/%s/releases?per_page=50", apiURL, url.PathEscape(m.owner), url.PathEscape(m.repo))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	setGitHubHeaders(req, m.githubToken())
	if cached.etag != "" && len(cached.releases) > 0 {
		req.Header.Set("If-None-Match", cached.etag)
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		if len(cached.releases) > 0 {
			return cached.releases, nil
		}
		return nil, fmt.Errorf("fetch GitHub releases: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified && len(cached.releases) > 0 {
		m.mu.Lock()
		m.cache.fetchedAt = time.Now()
		m.mu.Unlock()
		return cached.releases, nil
	}
	if resp.StatusCode != http.StatusOK {
		if len(cached.releases) > 0 {
			return cached.releases, nil
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("fetch GitHub releases: %s: %s%s", resp.Status, strings.TrimSpace(string(body)), rateLimitHint(resp))
	}

	var releases []Release
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("decode GitHub releases: %w", err)
	}

	m.mu.Lock()
	m.cache = releaseCache{releases: releases, etag: resp.Header.Get("ETag"), fetchedAt: time.Now()}
	m.mu.Unlock()
	return releases, nil
}

// rateLimitHint turns a 403/429 rate-limit response into actionable advice.
func rateLimitHint(resp *http.Response) string {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return ""
	}
	if resp.Header.Get("X-RateLimit-Remaining") != "0" {
		return ""
	}
	hint := " (GitHub API rate limit exhausted"
	if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
		if secs, err := strconv.ParseInt(reset, 10, 64); err == nil {
			hint += fmt.Sprintf("; resets at %s", time.Unix(secs, 0).UTC().Format(time.RFC3339))
		}
	}
	return hint + "; add a GitHub token in Settings, or install from a URL/upload)"
}

func (m *Manager) set(status Status) {
	m.mu.Lock()
	m.status = status
	m.mu.Unlock()
}

func (m *Manager) updateInstallProgress(progress InstallProgress) {
	status := m.Status()
	status.State = "installing"
	status.Phase = progress.Phase
	status.Message = progress.Message
	if progress.ProgressPercent > status.ProgressPercent {
		status.ProgressPercent = progress.ProgressPercent
	}
	m.set(status)
}

func (m *Manager) updateDownloadProgress(progress downloadProgress) {
	status := m.Status()
	status.DownloadedBytes = progress.downloaded
	status.TotalBytes = progress.total
	status.DownloadSpeedBps = progress.speedBps
	if progress.total > 0 {
		downloadPercent := (float64(progress.downloaded) / float64(progress.total)) * 75
		status.ProgressPercent = minFloat64(85, maxFloat64(status.ProgressPercent, 10+downloadPercent))
	}
	if status.Phase == "" || status.Phase == "downloading" {
		status.Phase = "flashing"
		status.Message = "Downloading and flashing OTA image"
		status.State = "installing"
	}
	m.set(status)
}

func (m *Manager) fail(err error) {
	status := m.Status()
	status.State = "failed"
	status.Phase = "failed"
	status.Error = err.Error()
	status.Message = "OTA installation failed"
	status.FinishedAt = time.Now()
	m.set(status)
	m.recordInstallHistory(m.historyFromStatus(status))
}

func (m *Manager) resolveInstaller() Installer {
	if m.installer != nil {
		return m.installer
	}
	updateURL := m.updateURL()
	return GokrazyInstaller{
		BaseURL:            updateURL,
		HTTPClient:         NewUpdateHTTPClient(updateURL, 30*time.Minute, m.insecure),
		InsecureSkipVerify: m.insecure,
	}
}

func (m *Manager) updateURL() string {
	if override := strings.TrimSpace(os.Getenv("OTA_GOKRAZY_UPDATE_URL")); override != "" {
		return override
	}
	pw := ""
	if m.password != nil {
		pw = m.password()
	}
	u := &url.URL{
		Scheme: "http",
		User:   url.UserPassword("gokrazy", pw),
		Host:   "127.0.0.1",
		Path:   "/",
	}
	return u.String()
}

func reportInstallProgress(progress InstallProgressFunc, phase, message string, progressPercent float64) {
	if progress == nil {
		return
	}
	progress(InstallProgress{
		Phase:           phase,
		Message:         message,
		ProgressPercent: progressPercent,
	})
}

type downloadProgress struct {
	downloaded int64
	total      int64
	speedBps   float64
}

type downloadProgressReader struct {
	reader         io.Reader
	total          int64
	started        time.Time
	lastReport     time.Time
	downloaded     int64
	reportProgress func(downloadProgress)
}

func newDownloadProgressReader(reader io.Reader, total int64, reportProgress func(downloadProgress)) *downloadProgressReader {
	now := time.Now()
	return &downloadProgressReader{
		reader:         reader,
		total:          total,
		started:        now,
		lastReport:     now,
		reportProgress: reportProgress,
	}
}

func (r *downloadProgressReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.downloaded += int64(n)
		r.report(false)
	}
	if err != nil {
		r.report(true)
	}
	return n, err
}

func (r *downloadProgressReader) report(force bool) {
	if r.reportProgress == nil {
		return
	}
	now := time.Now()
	if !force && now.Sub(r.lastReport) < time.Second && (r.total <= 0 || r.downloaded < r.total) {
		return
	}
	elapsed := now.Sub(r.started).Seconds()
	speed := 0.0
	if elapsed > 0 {
		speed = float64(r.downloaded) / elapsed
	}
	r.lastReport = now
	r.reportProgress(downloadProgress{
		downloaded: r.downloaded,
		total:      r.total,
		speedBps:   speed,
	})
}

func minFloat64(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func maxFloat64(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func findAsset(assets []Asset, name string) *Asset {
	for i := range assets {
		if assets[i].Name == name {
			return &assets[i]
		}
	}
	return nil
}

func envDefault(key, fallback string) string {
	return valueDefault(os.Getenv(key), fallback)
}

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	switch strings.ToLower(value) {
	case "1", "true", "t", "yes", "y", "on":
		return true
	case "0", "false", "f", "no", "n", "off":
		return false
	default:
		return fallback
	}
}

func valueDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

// NewUpdateHTTPClient returns an HTTP client configured for gokrazy updater
// uploads. It transparently follows the gokrazy HTTP→HTTPS redirect with the
// same Basic-Auth credentials, and skips TLS verification for loopback
// targets (gokrazy serves a self-signed cert there).
func NewUpdateHTTPClient(rawURL string, timeout time.Duration, insecureSkipVerify bool) *http.Client {
	return configureUpdateHTTPClient(&http.Client{Timeout: timeout}, rawURL, insecureSkipVerify)
}

// NewUpdateTarget returns a gokrazy updater target after resolving gokrazy's
// HTTP-to-HTTPS redirect. Uploads stream request bodies, so following a 302
// during PUT would turn the upload into a GET and fail at /update/root.
func NewUpdateTarget(ctx context.Context, rawBaseURL string, client *http.Client) (*updater.Target, error) {
	baseURL := normalizeUpdateBaseURL(rawBaseURL)
	if baseURL == "" {
		return nil, errors.New("ota: empty gokrazy updater base URL")
	}
	if client == nil {
		client = http.DefaultClient
	}
	resolvedBaseURL, err := resolveUpdateBaseURL(ctx, baseURL, client)
	if err != nil {
		return nil, err
	}
	return updater.NewTarget(ctx, resolvedBaseURL, client)
}

func normalizeUpdateBaseURL(rawURL string) string {
	baseURL := strings.TrimSpace(rawURL)
	if baseURL == "" || strings.HasSuffix(baseURL, "/") {
		return baseURL
	}
	return baseURL + "/"
}

func resolveUpdateBaseURL(ctx context.Context, baseURL string, client *http.Client) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"update/features", nil)
	if err != nil {
		return "", err
	}
	noRedirectClient := *client
	noRedirectClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return baseURL, nil
	}
	location := strings.TrimSpace(resp.Header.Get("Location"))
	if location == "" {
		return baseURL, nil
	}
	redirectURL, err := req.URL.Parse(location)
	if err != nil {
		return "", err
	}
	if redirectURL.User == nil {
		redirectURL.User = req.URL.User
	}
	redirectURL.RawQuery = ""
	redirectURL.Fragment = ""
	redirectURL.Path = strings.TrimSuffix(redirectURL.Path, "update/features")
	if redirectURL.Path == "" {
		redirectURL.Path = "/"
	}
	if !strings.HasSuffix(redirectURL.Path, "/") {
		redirectURL.Path += "/"
	}
	return redirectURL.String(), nil
}

func configureUpdateHTTPClient(client *http.Client, rawURL string, insecureSkipVerify bool) *http.Client {
	if !insecureSkipVerify && !shouldSkipUpdateTLSVerify(rawURL) {
		return client
	}
	cloned := *client
	var transport *http.Transport
	if client.Transport == nil {
		transport = http.DefaultTransport.(*http.Transport).Clone()
	} else {
		existing, ok := client.Transport.(*http.Transport)
		if !ok {
			return client
		}
		transport = existing.Clone()
	}
	tlsConfig := &tls.Config{}
	if transport.TLSClientConfig != nil {
		tlsConfig = transport.TLSClientConfig.Clone()
	}
	// #nosec G402 -- explicit OTA updater option; loopback gokrazy updater uses self-signed TLS
	tlsConfig.InsecureSkipVerify = true
	transport.TLSClientConfig = tlsConfig
	cloned.Transport = transport
	configureLoopbackRedirectAuth(&cloned, rawURL)
	return &cloned
}

func configureLoopbackRedirectAuth(client *http.Client, rawURL string) {
	baseURL, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || baseURL.User == nil || !isLoopbackHost(baseURL.Hostname()) {
		return
	}
	username := baseURL.User.Username()
	password, _ := baseURL.User.Password()
	checkRedirect := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 0 && isLoopbackHost(req.URL.Hostname()) && req.Header.Get("Authorization") == "" {
			req.SetBasicAuth(username, password)
		}
		if checkRedirect != nil {
			return checkRedirect(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
}

func shouldSkipUpdateTLSVerify(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return isLoopbackHost(u.Hostname())
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(strings.TrimSuffix(host, "."))
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
