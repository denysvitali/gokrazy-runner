package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// SystemOptions configures the /api/system and /api/logs endpoints. All
// fields are optional; defaults() fills in the on-device values.
type SystemOptions struct {
	PodmanBinary  string
	ContainerName string
	PermDir       string

	// Test hooks. When nil, real implementations are used.
	runCmd     func(ctx context.Context, name string, args ...string) ([]byte, error)
	readFile   func(path string) ([]byte, error)
	interfaces func() ([]net.Interface, error)
	hostname   func() string
	statfs     func(path string) (total, free uint64, err error)
	readKmsg   func(maxBytes int) (string, error)
}

const (
	systemCommandTimeout = 5 * time.Second
	// maxLogLines bounds /api/logs so a caller can't ask the device to
	// buffer an unbounded podman log into memory.
	maxLogLines     = 2000
	defaultLogLines = 200
)

func (o *SystemOptions) defaults() {
	if o.PodmanBinary == "" {
		o.PodmanBinary = "/user/podman"
	}
	if o.ContainerName == "" {
		o.ContainerName = "gokrazy-runner"
	}
	if o.PermDir == "" {
		o.PermDir = "/perm"
	}
	if o.runCmd == nil {
		o.runCmd = realRunCmd
	}
	if o.readFile == nil {
		o.readFile = os.ReadFile
	}
	if o.interfaces == nil {
		o.interfaces = net.Interfaces
	}
	if o.hostname == nil {
		o.hostname = realHostname
	}
	if o.statfs == nil {
		o.statfs = realStatfs
	}
	if o.readKmsg == nil {
		o.readKmsg = realReadKmsg
	}
}

// SystemInfo is the payload of GET /api/system: everything an operator
// would otherwise have to open a breakglass shell to find out.
type SystemInfo struct {
	Hostname   string      `json:"hostname"`
	Model      string      `json:"model,omitempty"`
	Kernel     string      `json:"kernel,omitempty"`
	Version    string      `json:"version"`
	BuildDate  string      `json:"build_date,omitempty"`
	UptimeSecs float64     `json:"uptime_seconds"`
	LoadAvg    []float64   `json:"load_average,omitempty"`
	CPUTempC   *float64    `json:"cpu_temp_c,omitempty"`
	Memory     *MemoryInfo `json:"memory,omitempty"`
	Disks      []DiskInfo  `json:"disks,omitempty"`
	Interfaces []IfaceInfo `json:"interfaces,omitempty"`
	Runner     RunnerState `json:"runner"`
}

// MemoryInfo reports RAM in bytes. Available is the kernel's own estimate
// of what a new workload could claim, which is the number that matters on
// a 1–8 GB Pi; Free alone understates it badly once the page cache warms.
type MemoryInfo struct {
	TotalBytes     uint64 `json:"total_bytes"`
	FreeBytes      uint64 `json:"free_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
}

// DiskInfo reports a mount point's capacity.
type DiskInfo struct {
	Path       string `json:"path"`
	TotalBytes uint64 `json:"total_bytes"`
	FreeBytes  uint64 `json:"free_bytes"`
}

// IfaceInfo is one network interface as shown in the UI.
type IfaceInfo struct {
	Name      string   `json:"name"`
	MAC       string   `json:"mac,omitempty"`
	Up        bool     `json:"up"`
	Addresses []string `json:"addresses,omitempty"`
}

// RunnerState describes the actions-runner container.
type RunnerState struct {
	// Status is one of: running, stopped, absent, unknown.
	Status    string `json:"status"`
	Container string `json:"container"`
	Image     string `json:"image,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	opts := s.cfg.System
	opts.defaults()
	writeJSON(w, http.StatusOK, opts.collect(r.Context(), s.cfg))
}

func (o *SystemOptions) collect(ctx context.Context, server ServerConfig) SystemInfo {
	info := SystemInfo{
		Hostname:   o.hostname(),
		Model:      o.readModel(),
		Kernel:     o.readKernel(),
		Version:    firstNonEmpty(server.Version, "unknown"),
		UptimeSecs: o.readUptimeSeconds(),
		LoadAvg:    o.readLoadAvg(),
		CPUTempC:   o.readCPUTemp(),
		Memory:     o.readMemory(),
		Disks:      o.readDisks(),
		Interfaces: o.readInterfaces(),
		Runner:     o.readRunnerState(ctx),
	}
	return info
}

func (o *SystemOptions) readModel() string {
	// The device tree exposes a friendly board name ("Raspberry Pi 4
	// Model B Rev 1.4"); it is NUL-terminated, unlike a normal procfs file.
	if raw, err := o.readFile("/proc/device-tree/model"); err == nil {
		return strings.TrimSpace(strings.TrimRight(string(raw), "\x00"))
	}
	return ""
}

func (o *SystemOptions) readKernel() string {
	if raw, err := o.readFile("/proc/sys/kernel/osrelease"); err == nil {
		return strings.TrimSpace(string(raw))
	}
	return ""
}

func (o *SystemOptions) readUptimeSeconds() float64 {
	raw, err := o.readFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return 0
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return secs
}

func (o *SystemOptions) readLoadAvg() []float64 {
	raw, err := o.readFile("/proc/loadavg")
	if err != nil {
		return nil
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 3 {
		return nil
	}
	out := make([]float64, 0, 3)
	for _, f := range fields[:3] {
		v, err := strconv.ParseFloat(f, 64)
		if err != nil {
			return nil
		}
		out = append(out, v)
	}
	return out
}

// readCPUTemp reads the SoC thermal zone, reported in millidegrees C.
// Returns nil when the board exposes no thermal zone, so the UI can hide
// the tile rather than show a bogus 0 °C.
func (o *SystemOptions) readCPUTemp() *float64 {
	raw, err := o.readFile("/sys/class/thermal/thermal_zone0/temp")
	if err != nil {
		return nil
	}
	milli, err := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64)
	if err != nil {
		return nil
	}
	c := milli / 1000
	return &c
}

func (o *SystemOptions) readMemory() *MemoryInfo {
	raw, err := o.readFile("/proc/meminfo")
	if err != nil {
		return nil
	}
	var mem MemoryInfo
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}
		// meminfo values are in kB except for a few HugePages counters,
		// none of which we read here.
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "MemTotal":
			mem.TotalBytes = kb * 1024
		case "MemFree":
			mem.FreeBytes = kb * 1024
		case "MemAvailable":
			mem.AvailableBytes = kb * 1024
		}
	}
	if mem.TotalBytes == 0 {
		return nil
	}
	return &mem
}

func (o *SystemOptions) readDisks() []DiskInfo {
	var out []DiskInfo
	for _, path := range []string{"/", o.PermDir} {
		total, free, err := o.statfs(path)
		if err != nil || total == 0 {
			continue
		}
		out = append(out, DiskInfo{Path: path, TotalBytes: total, FreeBytes: free})
	}
	return out
}

func (o *SystemOptions) readInterfaces() []IfaceInfo {
	ifaces, err := o.interfaces()
	if err != nil {
		return nil
	}
	out := make([]IfaceInfo, 0, len(ifaces))
	for _, ifc := range ifaces {
		// Loopback tells an operator nothing about connectivity.
		if ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		entry := IfaceInfo{
			Name: ifc.Name,
			MAC:  ifc.HardwareAddr.String(),
			Up:   ifc.Flags&net.FlagUp != 0,
		}
		if addrs, err := ifc.Addrs(); err == nil {
			for _, a := range addrs {
				entry.Addresses = append(entry.Addresses, a.String())
			}
		}
		out = append(out, entry)
	}
	return out
}

// podmanInspect is the subset of `podman ps --format json` we consume.
type podmanPSEntry struct {
	Names     []string `json:"Names"`
	Image     string   `json:"Image"`
	State     string   `json:"State"`
	Status    string   `json:"Status"`
	StartedAt int64    `json:"StartedAt"`
}

// readRunnerState asks podman about the runner container. Anything that
// goes wrong is reported as "unknown" with a detail string rather than an
// error: the status card should degrade, not disappear.
func (o *SystemOptions) readRunnerState(ctx context.Context) RunnerState {
	state := RunnerState{Status: "unknown", Container: o.ContainerName}

	if _, err := os.Stat(o.PodmanBinary); errors.Is(err, os.ErrNotExist) {
		state.Detail = "podman not present at " + o.PodmanBinary
		return state
	}

	cmdCtx, cancel := context.WithTimeout(ctx, systemCommandTimeout)
	defer cancel()

	out, err := o.runCmd(cmdCtx, o.PodmanBinary, "ps", "--all", "--format", "json",
		"--filter", "name=^"+o.ContainerName+"$")
	if err != nil {
		state.Detail = strings.TrimSpace(string(out))
		if state.Detail == "" {
			state.Detail = err.Error()
		}
		return state
	}

	var entries []podmanPSEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		state.Detail = "unparseable podman output: " + err.Error()
		return state
	}
	if len(entries) == 0 {
		state.Status = "absent"
		state.Detail = "no container named " + o.ContainerName
		return state
	}

	entry := entries[0]
	state.Image = entry.Image
	state.Detail = entry.Status
	if strings.EqualFold(entry.State, "running") {
		state.Status = "running"
	} else {
		state.Status = "stopped"
	}
	if entry.StartedAt > 0 {
		state.StartedAt = time.Unix(entry.StartedAt, 0).UTC().Format(time.RFC3339)
	}
	return state
}

// handleRunnerRestart force-removes the runner container. runner-init
// notices the exit and starts a fresh one within its backoff window, so
// this is a restart rather than a stop.
func (s *Server) handleRunnerRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	opts := s.cfg.System
	opts.defaults()

	if _, err := os.Stat(opts.PodmanBinary); errors.Is(err, os.ErrNotExist) {
		http.Error(w, "podman not present at "+opts.PodmanBinary, http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), systemCommandTimeout)
	defer cancel()

	out, err := opts.runCmd(ctx, opts.PodmanBinary, "rm", "-f", opts.ContainerName)
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		http.Error(w, detail, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
		"detail": "container removed; runner-init will start a new one shortly",
	})
}

// handleLogs streams a bounded tail of either the runner container log or
// the kernel ring buffer. Plain text so it can be copied straight into a
// bug report.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	opts := s.cfg.System
	opts.defaults()

	lines := defaultLogLines
	if raw := r.URL.Query().Get("lines"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			http.Error(w, "lines must be a positive integer", http.StatusBadRequest)
			return
		}
		lines = min(n, maxLogLines)
	}

	var (
		body string
		err  error
	)
	switch source := r.URL.Query().Get("source"); source {
	case "", "runner":
		body, err = opts.runnerLogs(r.Context(), lines)
	case "kernel":
		body, err = opts.readKmsg(supportMaxKmsgBytes)
		body = tailLines(body, lines)
	default:
		http.Error(w, "unknown log source "+source, http.StatusBadRequest)
		return
	}
	if err != nil && body == "" {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err != nil {
		// podman exits non-zero when the container is gone, but whatever it
		// printed is still the most useful thing we can show.
		body += "\n(" + err.Error() + ")\n"
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(body))
}

func (o *SystemOptions) runnerLogs(ctx context.Context, lines int) (string, error) {
	if _, err := os.Stat(o.PodmanBinary); errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("podman not present at %s", o.PodmanBinary)
	}
	cmdCtx, cancel := context.WithTimeout(ctx, systemCommandTimeout)
	defer cancel()

	out, err := o.runCmd(cmdCtx, o.PodmanBinary, "logs", "--tail", strconv.Itoa(lines), o.ContainerName)
	return string(out), err
}

func realStatfs(path string) (total, free uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	// Bsize is the preferred I/O block size, which is what the block counts
	// in statfs are denominated in.
	bs := uint64(st.Bsize) // #nosec G115 -- Bsize is a small positive block size
	return st.Blocks * bs, st.Bavail * bs, nil
}
