package webui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/denysvitali/gokrazy-runner/pkg/ota"
)

const goZeroPseudoVersion = "v0.0.0-00010101000000-000000000000"

// otaUploadDir is where uploaded images are spooled. /perm is disk-backed;
// /tmp on gokrazy is RAM and too small for a root image.
var otaUploadDir = "/perm"

type otaReleaseCandidate struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	PublishedAt time.Time `json:"published_at"`
	AssetName   string    `json:"asset_name"`
	AssetSize   int64     `json:"asset_size"`
	AssetURL    string    `json:"asset_url"`
	HTMLURL     string    `json:"html_url"`
	Installed   bool      `json:"installed"`
}

type otaInstallHistoryItem struct {
	Release    string `json:"release"`
	Asset      string `json:"asset"`
	AssetURL   string `json:"asset_url"`
	State      string `json:"state"`
	Message    string `json:"message,omitempty"`
	Error      string `json:"error,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
}

type otaABPartitionMeta struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	SizeHuman string `json:"size_human"`
}

type otaABPartitionStatus struct {
	Active       string             `json:"active"`
	Inactive     string             `json:"inactive"`
	UpdateSlot   string             `json:"update_slot"`
	Source       string             `json:"source"`
	ActiveInfo   otaABPartitionMeta `json:"active_info"`
	InactiveInfo otaABPartitionMeta `json:"inactive_info"`
	UpdateInfo   otaABPartitionMeta `json:"update_info"`
}

type otaStatusResponse struct {
	ota.Status
	CurrentVersion    string                  `json:"current_version"`
	ABPartitions      otaABPartitionStatus    `json:"ab_partitions"`
	LatestVersion     string                  `json:"latest_version"`
	InstalledVersions []string                `json:"installed_versions"`
	Releases          []otaReleaseCandidate   `json:"releases"`
	InstallHistory    []otaInstallHistoryItem `json:"install_history"`
	UpdateAvailable   bool                    `json:"update_available"`
	ReleasesError     string                  `json:"releases_error,omitempty"`
	HasGitHubToken    bool                    `json:"has_github_token"`
}

type otaInstallRequest struct {
	ReleaseTag string `json:"release_tag"`
	// URL installs a gzipped squashfs from an arbitrary http(s) location
	// instead of going through the GitHub releases API.
	URL string `json:"url"`
}

type otaTokenRequest struct {
	Token string `json:"token"`
}

func (s *Server) handleOTAStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.OTAMgr == nil {
		http.Error(w, "OTA manager not initialized", http.StatusServiceUnavailable)
		return
	}

	current := s.cfg.Version
	response := otaStatusResponse{
		Status:         s.cfg.OTAMgr.Status(),
		CurrentVersion: current,
		ABPartitions:   getABPartitions(),
		HasGitHubToken: s.cfg.OTAMgr.HasGitHubToken(),
	}
	for _, name := range collectKnownVersions(s.cfg.OTAMgr, current) {
		if name != "" {
			response.InstalledVersions = append(response.InstalledVersions, name)
		}
	}
	response.InstallHistory = collectInstallHistory(s.cfg.OTAMgr)

	releases, err := s.cfg.OTAMgr.AvailableReleases(r.Context())
	if err != nil {
		response.ReleasesError = err.Error()
	} else {
		response.Releases = make([]otaReleaseCandidate, 0, len(releases))
		for _, candidate := range releases {
			asset := candidate.Assets[0]
			option := otaReleaseCandidate{
				TagName:     candidate.TagName,
				Name:        candidate.Name,
				PublishedAt: candidate.PublishedAt,
				AssetName:   asset.Name,
				AssetSize:   asset.Size,
				AssetURL:    asset.BrowserDownloadURL,
				HTMLURL:     candidate.HTMLURL,
			}
			if sameReleaseVersion(option.TagName, current) {
				option.Installed = true
			}
			if response.LatestVersion == "" {
				response.LatestVersion = option.TagName
			}
			if !response.UpdateAvailable {
				response.UpdateAvailable = isReleaseUpdateAvailable(current, option.TagName)
			}
			response.Releases = append(response.Releases, option)
		}
	}

	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleOTAInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.OTAMgr == nil {
		http.Error(w, "OTA manager not initialized", http.StatusServiceUnavailable)
		return
	}
	var req otaInstallRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "invalid OTA install request", http.StatusBadRequest)
			return
		}
	}
	var (
		status ota.Status
		err    error
	)
	if strings.TrimSpace(req.URL) != "" {
		status, err = s.cfg.OTAMgr.StartWithURL(r.Context(), req.URL)
	} else {
		status, err = s.cfg.OTAMgr.StartWithRelease(r.Context(), req.ReleaseTag)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusAccepted, status)
}

// maxOTAUploadBytes bounds an uploaded image. A root squashfs is well under
// 512 MiB; anything larger is a mistake or an attempt to fill /perm.
const maxOTAUploadBytes = 512 << 20

// otaUploadDeadline bounds how long a single image upload may take.
const otaUploadDeadline = 30 * time.Minute

// handleOTAUpload accepts a gzipped squashfs image as the raw request body
// (Content-Type: application/gzip) and installs it. The body is spooled to
// /perm first — the install outlives the request, and /tmp is RAM on gokrazy.
func (s *Server) handleOTAUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.OTAMgr == nil {
		http.Error(w, "OTA manager not initialized", http.StatusServiceUnavailable)
		return
	}
	if r.Body == nil {
		http.Error(w, "empty upload", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// A root image is hundreds of MiB; over a slow link the upload easily
	// outlives the server-wide 30s ReadTimeout. Push the per-request
	// deadlines out (transparently through TLS); if the connection doesn't
	// support deadlines we silently keep the server-wide timeout.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(otaUploadDeadline))
	_ = rc.SetWriteDeadline(time.Now().Add(otaUploadDeadline))

	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		name = "uploaded image"
	}

	spool, err := os.CreateTemp(otaUploadDir, "ota-upload-*.gz")
	if err != nil {
		http.Error(w, fmt.Sprintf("create upload spool: %v", err), http.StatusInternalServerError)
		return
	}
	spoolPath := spool.Name()
	written, err := io.Copy(spool, io.LimitReader(r.Body, maxOTAUploadBytes+1))
	closeErr := spool.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(spoolPath)
		http.Error(w, fmt.Sprintf("store upload: %v", err), http.StatusInternalServerError)
		return
	}
	if written > maxOTAUploadBytes {
		os.Remove(spoolPath)
		http.Error(w, "uploaded image exceeds 512 MiB", http.StatusRequestEntityTooLarge)
		return
	}
	if written == 0 {
		os.Remove(spoolPath)
		http.Error(w, "empty upload", http.StatusBadRequest)
		return
	}

	status, err := s.cfg.OTAMgr.StartWithFile(r.Context(), spoolPath, name, written)
	if err != nil {
		os.Remove(spoolPath)
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusAccepted, status)
}

// handleOTAToken stores (or clears) the GitHub API token used to lift the
// anonymous rate limit. The token is never returned.
func (s *Server) handleOTAToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.OTAMgr == nil {
		http.Error(w, "OTA manager not initialized", http.StatusServiceUnavailable)
		return
	}
	var req otaTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid token request", http.StatusBadRequest)
		return
	}
	if err := s.cfg.OTAMgr.SetGitHubToken(req.Token); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"has_github_token": s.cfg.OTAMgr.HasGitHubToken()})
}

func isReleaseUpdateAvailable(currentVersion, releaseTag string) bool {
	if sameReleaseVersion(releaseTag, currentVersion) {
		return false
	}
	currentVersion = strings.TrimSpace(currentVersion)
	if currentVersion == "" || currentVersion == "dev" || currentVersion == goZeroPseudoVersion {
		return strings.TrimSpace(releaseTag) != ""
	}
	return strings.TrimSpace(releaseTag) != ""
}

func sameReleaseVersion(a, b string) bool {
	return strings.TrimSpace(a) == strings.TrimSpace(b)
}

func collectKnownVersions(otaManager *ota.Manager, currentVersion string) []string {
	history := otaManager.InstallationHistory()
	knownVersions := make([]string, 0, len(history)+1)
	knownSet := map[string]struct{}{}

	for i := len(history) - 1; i >= 0; i-- {
		if history[i].State != "installed" {
			continue
		}
		release := strings.TrimSpace(history[i].Release)
		if !isKnownInstalledVersion(release) {
			continue
		}
		if _, exists := knownSet[release]; exists {
			continue
		}
		knownVersions = append(knownVersions, release)
		knownSet[release] = struct{}{}
	}

	if isKnownInstalledVersion(currentVersion) {
		if _, exists := knownSet[currentVersion]; !exists {
			knownVersions = append(knownVersions, currentVersion)
		}
	}
	return knownVersions
}

func isKnownInstalledVersion(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && value != "dev" && value != goZeroPseudoVersion
}

func collectInstallHistory(otaManager *ota.Manager) []otaInstallHistoryItem {
	history := otaManager.InstallationHistory()
	items := make([]otaInstallHistoryItem, 0, len(history))

	for i := len(history) - 1; i >= 0; i-- {
		entry := history[i]
		if entry.Release == "" && entry.Message == "" {
			continue
		}
		items = append(items, otaInstallHistoryItem{
			Release:    entry.Release,
			Asset:      entry.Asset,
			AssetURL:   entry.AssetURL,
			State:      entry.State,
			Message:    entry.Message,
			Error:      entry.Error,
			StartedAt:  formatTimeForJSON(entry.StartedAt),
			FinishedAt: formatTimeForJSON(entry.FinishedAt),
		})
	}
	return items
}

func formatTimeForJSON(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Format(time.RFC3339)
}

func getABPartitions() otaABPartitionStatus {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return otaABPartitionStatus{Source: "proc_cmdline_unavailable"}
	}

	for _, part := range strings.Fields(string(data)) {
		if !strings.HasPrefix(part, "root=") {
			continue
		}
		rootValue := strings.TrimSpace(strings.TrimPrefix(part, "root="))
		activePartition := parseRootPartition(rootValue)
		activePath := resolveRootPartitionPath(rootValue)
		activeMeta := partitionMetadataFor(activePath, activePartition)
		if activePartition == 0 {
			return otaABPartitionStatus{Source: "proc_cmdline_unrecognized_root"}
		}
		if activePartition != 2 && activePartition != 3 {
			return otaABPartitionStatus{Source: "proc_cmdline_no_ab_partition"}
		}
		active := strconv.Itoa(activePartition)
		inactive := "3"
		if activePartition == 3 {
			inactive = "2"
		}
		inactivePartition, _ := strconv.Atoi(inactive)
		inactiveMeta := partitionMetadataFor(inferPartitionPath(activePath, inactive), inactivePartition)
		return otaABPartitionStatus{
			Active:       active,
			Inactive:     inactive,
			UpdateSlot:   inactive,
			Source:       "proc_cmdline",
			ActiveInfo:   activeMeta,
			InactiveInfo: inactiveMeta,
			UpdateInfo:   inactiveMeta,
		}
	}
	return otaABPartitionStatus{Source: "proc_cmdline_root_missing"}
}

func parseRootPartition(rootValue string) int {
	rootValue = strings.TrimSpace(rootValue)
	if rootValue == "" {
		return 0
	}
	if strings.HasPrefix(rootValue, "PARTUUID=") {
		partUUID := strings.TrimPrefix(rootValue, "PARTUUID=")
		if part, ok := parsePARTNROFFPartition(partUUID); ok {
			return part
		}
		lastDash := strings.LastIndex(partUUID, "-")
		if lastDash >= 0 {
			if part, err := strconv.Atoi(partUUID[lastDash+1:]); err == nil && part > 0 {
				return part
			}
		}
	}
	lastP := strings.LastIndex(rootValue, "p")
	if lastP >= 0 && lastP+1 < len(rootValue) {
		if part, err := strconv.Atoi(rootValue[lastP+1:]); err == nil {
			return part
		}
	}
	lastDigitStart := len(rootValue)
	for lastDigitStart > 0 && rootValue[lastDigitStart-1] >= '0' && rootValue[lastDigitStart-1] <= '9' {
		lastDigitStart--
	}
	if lastDigitStart < len(rootValue) {
		if part, err := strconv.Atoi(rootValue[lastDigitStart:]); err == nil {
			return part
		}
	}
	return 0
}

func parsePARTNROFFPartition(partUUID string) (int, bool) {
	const marker = "/PARTNROFF="
	partUUID = strings.TrimSpace(partUUID)
	idx := strings.LastIndex(partUUID, marker)
	if idx < 0 {
		return 0, false
	}
	offsetValue := strings.TrimSpace(partUUID[idx+len(marker):])
	if offsetValue == "" {
		return 0, false
	}
	offset, err := strconv.Atoi(offsetValue)
	if err != nil || offset < 0 {
		return 0, false
	}
	return 1 + offset, true
}

func partitionMetadataFor(rootValue string, partition int) otaABPartitionMeta {
	if partition <= 0 {
		return otaABPartitionMeta{}
	}
	if rootValue == "" {
		return otaABPartitionMeta{SizeHuman: "unknown"}
	}
	size := partitionSizeBytes(rootValue)
	return otaABPartitionMeta{
		Path:      rootValue,
		SizeBytes: size,
		SizeHuman: formatBytes(size),
	}
}

func inferPartitionPath(rootValue, partition string) string {
	if partition == "" || rootValue == "" {
		return ""
	}
	n := len(rootValue)
	for n > 0 {
		if rootValue[n-1] >= '0' && rootValue[n-1] <= '9' {
			n--
			continue
		}
		break
	}
	if n >= len(rootValue) {
		return rootValue
	}
	return rootValue[:n] + partition
}

func resolveRootPartitionPath(rootValue string) string {
	rootValue = strings.TrimSpace(strings.Trim(rootValue, "\""))
	if rootValue == "" {
		return ""
	}
	if strings.HasPrefix(rootValue, "PARTUUID=") {
		partUUID := strings.TrimPrefix(rootValue, "PARTUUID=")
		partnoffPartition, hasPartnoff := parsePARTNROFFPartition(partUUID)
		if idx := strings.LastIndex(partUUID, "/PARTNROFF="); idx >= 0 {
			partUUID = partUUID[:idx]
		}
		resolved, err := filepath.EvalSymlinks(filepath.Join("/dev/disk/by-partuuid", partUUID))
		if err == nil {
			if hasPartnoff {
				return inferPartitionPath(resolved, strconv.Itoa(partnoffPartition))
			}
			return resolved
		}
	}
	if strings.HasPrefix(rootValue, "/dev/") {
		return rootValue
	}
	return ""
}

func partitionSizeBytes(path string) int64 {
	name := filepath.Base(path)
	if name == "." || name == "/" {
		return 0
	}
	candidates := []string{
		filepath.Join("/sys/class/block", name, "size"),
		filepath.Join("/sys/block", name, "size"),
	}
	for _, candidate := range candidates {
		raw, err := os.ReadFile(candidate) // #nosec G304 -- /sys path with vetted basename
		if err != nil {
			continue
		}
		parsed, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil || parsed <= 0 {
			continue
		}
		return parsed * 512
	}
	parent := filepath.Dir(path)
	if parent != "/" && parent != "." && parent != "" {
		alt := filepath.Base(parent)
		candidate := filepath.Join("/sys/block", alt, filepath.Base(path), "size")
		raw, err := os.ReadFile(candidate) // #nosec G304 -- /sys path with vetted basename
		if err != nil {
			return 0
		}
		parsed, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil || parsed <= 0 {
			return 0
		}
		return parsed * 512
	}
	return 0
}

func formatBytes(size int64) string {
	if size <= 0 {
		return "unknown"
	}
	if size < 1024 {
		return fmt.Sprintf("%d B", size)
	}
	if size < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(size)/1024)
	}
	if size < 1024*1024*1024 {
		return fmt.Sprintf("%.1f MB", float64(size)/(1024*1024))
	}
	return fmt.Sprintf("%.1f GB", float64(size)/(1024*1024*1024))
}
