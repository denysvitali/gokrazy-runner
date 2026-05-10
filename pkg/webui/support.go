package webui

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// SupportOptions configures the diagnostics endpoint. All paths are
// optional; sensible defaults are filled in when zero.
type SupportOptions struct {
	EnvPath          string
	TokenPath        string
	TailscaleKeyPath string
	PermDir          string
	OTAHistoryPath   string

	PodmanBinary    string
	TailscaleBinary string
	ContainerName   string

	// Test hooks. When nil, real implementations are used.
	runCmd     func(ctx context.Context, name string, args ...string) ([]byte, error)
	readFile   func(path string) ([]byte, error)
	now        func() time.Time
	interfaces func() ([]net.Interface, error)
	readKmsg   func(maxBytes int) (string, error)
	listDir    func(path string) ([]os.DirEntry, error)
	hostname   func() string
}

const (
	supportMaxKmsgBytes      = 96 * 1024
	supportKmsgTailLines     = 200
	supportContainerTailLogs = "200"
	supportCommandTimeout    = 5 * time.Second
)

func (o *SupportOptions) defaults() {
	if o.PodmanBinary == "" {
		o.PodmanBinary = "/user/podman"
	}
	if o.TailscaleBinary == "" {
		o.TailscaleBinary = "/user/tailscale"
	}
	if o.ContainerName == "" {
		o.ContainerName = "gokrazy-runner"
	}
	if o.PermDir == "" {
		o.PermDir = "/perm"
	}
	if o.OTAHistoryPath == "" {
		o.OTAHistoryPath = "/perm/ota-install-history.json"
	}
	if o.runCmd == nil {
		o.runCmd = realRunCmd
	}
	if o.readFile == nil {
		o.readFile = os.ReadFile
	}
	if o.now == nil {
		o.now = time.Now
	}
	if o.interfaces == nil {
		o.interfaces = net.Interfaces
	}
	if o.readKmsg == nil {
		o.readKmsg = realReadKmsg
	}
	if o.listDir == nil {
		o.listDir = os.ReadDir
	}
	if o.hostname == nil {
		o.hostname = realHostname
	}
}

func (s *Server) handleSupport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	bundle := s.cfg.Support
	bundle.defaults()
	out := bundle.collect(r.Context(), s.cfg)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="gokrazy-runner-support.txt"`)
	_, _ = io.WriteString(w, out)
}

func (o *SupportOptions) collect(ctx context.Context, server ServerConfig) string {
	envPath := firstNonEmpty(o.EnvPath, server.EnvPath)
	tokenPath := firstNonEmpty(o.TokenPath, server.TokenPath)
	tsKeyPath := firstNonEmpty(o.TailscaleKeyPath, server.TailscaleKeyPath)

	var b strings.Builder
	writeHeader(&b, o, server)

	o.section(&b, "/etc/resolv.conf", func() (string, error) {
		return readTextFile(o.readFile, "/etc/resolv.conf")
	})
	o.section(&b, "/etc/hosts", func() (string, error) {
		return readTextFile(o.readFile, "/etc/hosts")
	})
	o.section(&b, "/etc/nsswitch.conf", func() (string, error) {
		return readTextFile(o.readFile, "/etc/nsswitch.conf")
	})
	o.section(&b, "Network interfaces", func() (string, error) {
		return describeInterfaces(o.interfaces)
	})
	o.section(&b, "/proc/net/route", func() (string, error) {
		return readTextFile(o.readFile, "/proc/net/route")
	})
	o.section(&b, "/proc/net/ipv6_route", func() (string, error) {
		return readTextFile(o.readFile, "/proc/net/ipv6_route")
	})
	o.section(&b, "runner.env (redacted)", func() (string, error) {
		return readRedactedRunnerEnv(o.readFile, envPath)
	})
	o.section(&b, "runner.token", func() (string, error) {
		return describeFilePresence(o.readFile, tokenPath)
	})
	o.section(&b, "tailscale.authkey", func() (string, error) {
		return describeFilePresence(o.readFile, tsKeyPath)
	})
	o.section(&b, "/perm listing", func() (string, error) {
		return listPerm(o.listDir, o.PermDir)
	})
	o.section(&b, "OTA install history", func() (string, error) {
		return readTextFile(o.readFile, o.OTAHistoryPath)
	})
	o.section(&b, "Kernel log (last "+itoa(supportKmsgTailLines)+" lines)", func() (string, error) {
		raw, err := o.readKmsg(supportMaxKmsgBytes)
		if err != nil {
			return raw, err
		}
		return tailLines(raw, supportKmsgTailLines), nil
	})
	o.section(&b, "podman ps -a", func() (string, error) {
		return runWithTimeout(ctx, o.runCmd, o.PodmanBinary, "ps", "-a")
	})
	o.section(&b, "podman images", func() (string, error) {
		return runWithTimeout(ctx, o.runCmd, o.PodmanBinary, "images")
	})
	o.section(&b, "podman logs "+o.ContainerName+" (last "+supportContainerTailLogs+" lines)", func() (string, error) {
		return runWithTimeout(ctx, o.runCmd, o.PodmanBinary, "logs", "--tail", supportContainerTailLogs, o.ContainerName)
	})
	o.section(&b, "tailscale status", func() (string, error) {
		return runWithTimeout(ctx, o.runCmd, o.TailscaleBinary, "status")
	})
	o.section(&b, "tailscale netcheck", func() (string, error) {
		return runWithTimeout(ctx, o.runCmd, o.TailscaleBinary, "netcheck")
	})
	return b.String()
}

func writeHeader(b *strings.Builder, o *SupportOptions, server ServerConfig) {
	host := o.hostname()
	if host == "" {
		host = "<unknown>"
	}
	fmt.Fprintf(b, "gokrazy-runner support bundle\n")
	fmt.Fprintf(b, "generated: %s\n", o.now().UTC().Format(time.RFC3339))
	fmt.Fprintf(b, "hostname:  %s\n", host)
	fmt.Fprintf(b, "version:   %s\n", firstNonEmpty(server.Version, "unknown"))
	fmt.Fprintf(b, "uptime:    %s\n", readUptime(o.readFile))
	b.WriteString("\n")
}

func (o *SupportOptions) section(b *strings.Builder, title string, fn func() (string, error)) {
	fmt.Fprintf(b, "==== %s ====\n", title)
	body, err := fn()
	body = strings.TrimRight(body, "\n")
	if body != "" {
		b.WriteString(body)
		b.WriteString("\n")
	}
	if err != nil {
		fmt.Fprintf(b, "(error: %v)\n", err)
	}
	if body == "" && err == nil {
		b.WriteString("(empty)\n")
	}
	b.WriteString("\n")
}

// readRedactedRunnerEnv reads runner.env and returns it with sensitive
// values masked. Reserved runner keys (URL/NAME/LABELS/IMAGE) are kept
// verbatim because they are useful for diagnosis and can't carry secrets;
// any other key whose name looks sensitive (TOKEN/SECRET/KEY/PASSWORD/...)
// has its value replaced with a fixed-width mask.
func readRedactedRunnerEnv(readFile func(string) ([]byte, error), path string) (string, error) {
	if path == "" {
		return "(no env path configured)", nil
	}
	raw, err := readFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "(not present)", nil
		}
		return "", err
	}
	var out strings.Builder
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		line := sc.Text()
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			out.WriteString(line)
			out.WriteByte('\n')
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			out.WriteString(line)
			out.WriteByte('\n')
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := line[eq+1:]
		val = strings.TrimSpace(stripQuotes(strings.TrimSpace(val)))
		if isSensitiveEnvKey(key) {
			out.WriteString(key)
			out.WriteString("=")
			out.WriteString(maskValue(val))
			out.WriteByte('\n')
			continue
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		return out.String(), err
	}
	return out.String(), nil
}

var sensitiveEnvSubstrings = []string{
	"TOKEN", "SECRET", "PASSWORD", "PASSWD", "AUTH",
	"CREDENTIAL", "COOKIE", "SESSION", "PRIVATE",
}

func isSensitiveEnvKey(key string) bool {
	if _, reserved := reservedKeys[key]; reserved {
		return false
	}
	upper := strings.ToUpper(key)
	for _, needle := range sensitiveEnvSubstrings {
		if strings.Contains(upper, needle) {
			return true
		}
	}
	// Anchor *_KEY at an underscore boundary so we catch API_KEY /
	// AUTH_KEY but not MONKEY. AUTHKEY-style names hit via the AUTH
	// substring above.
	if upper == "KEY" || strings.HasSuffix(upper, "_KEY") {
		return true
	}
	return false
}

func maskValue(v string) string {
	if v == "" {
		return "(empty)"
	}
	return fmt.Sprintf("**redacted (%d bytes)**", len(v))
}

func describeFilePresence(readFile func(string) ([]byte, error), path string) (string, error) {
	if path == "" {
		return "(path not configured)", nil
	}
	raw, err := readFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Sprintf("not present at %s", path), nil
		}
		return "", err
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return fmt.Sprintf("present at %s but empty", path), nil
	}
	return fmt.Sprintf("present at %s (%d bytes)", path, len(raw)), nil
}

func readTextFile(readFile func(string) ([]byte, error), path string) (string, error) {
	raw, err := readFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "(not present)", nil
		}
		return "", err
	}
	return string(raw), nil
}

func describeInterfaces(list func() ([]net.Interface, error)) (string, error) {
	ifaces, err := list()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, ifc := range ifaces {
		fmt.Fprintf(&b, "%s: flags=%s mtu=%d", ifc.Name, ifc.Flags, ifc.MTU)
		if hw := ifc.HardwareAddr.String(); hw != "" {
			fmt.Fprintf(&b, " mac=%s", hw)
		}
		b.WriteByte('\n')
		addrs, err := ifc.Addrs()
		if err != nil {
			fmt.Fprintf(&b, "    (addrs error: %v)\n", err)
			continue
		}
		for _, a := range addrs {
			fmt.Fprintf(&b, "    %s\n", a.String())
		}
	}
	return b.String(), nil
}

func listPerm(read func(string) ([]os.DirEntry, error), root string) (string, error) {
	if root == "" {
		return "(no perm path configured)", nil
	}
	entries, err := read(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "(not present)", nil
		}
		return "", err
	}
	type row struct {
		name  string
		kind  string
		size  int64
		mtime time.Time
	}
	rows := make([]row, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		var size int64
		var mtime time.Time
		if err == nil {
			size = info.Size()
			mtime = info.ModTime()
		}
		kind := "file"
		switch {
		case e.IsDir():
			kind = "dir"
		case e.Type()&os.ModeSymlink != 0:
			kind = "link"
		}
		rows = append(rows, row{e.Name(), kind, size, mtime})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	var b strings.Builder
	for _, r := range rows {
		ts := ""
		if !r.mtime.IsZero() {
			ts = r.mtime.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(&b, "%-4s %10d %s  %s\n", r.kind, r.size, ts, filepath.Join(root, r.name))
	}
	return b.String(), nil
}

func runWithTimeout(parent context.Context, run func(context.Context, string, ...string) ([]byte, error),
	name string, args ...string,
) (string, error) {
	if _, err := os.Stat(name); errors.Is(err, os.ErrNotExist) {
		return "(binary not present at " + name + ")", nil
	}
	ctx, cancel := context.WithTimeout(parent, supportCommandTimeout)
	defer cancel()
	out, err := run(ctx, name, args...)
	body := string(out)
	if err != nil {
		// exec errors are common (container not running, etc.); the
		// captured output still has value for diagnosis.
		return body, err
	}
	return body, nil
}

func realRunCmd(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func realHostname() string {
	h, _ := os.Hostname()
	return h
}

func realReadKmsg(maxBytes int) (string, error) {
	f, err := os.OpenFile("/dev/kmsg", os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "(no /dev/kmsg)", nil
		}
		return "", err
	}
	defer f.Close()
	var buf bytes.Buffer
	record := make([]byte, 8192)
	for buf.Len() < maxBytes {
		n, err := f.Read(record)
		if n > 0 {
			line := record[:n]
			// kmsg record format: "<level>,seq,timestamp_us,...;message\n"
			if i := bytes.IndexByte(line, ';'); i >= 0 {
				line = line[i+1:]
			}
			buf.Write(line)
			if !bytes.HasSuffix(line, []byte{'\n'}) {
				buf.WriteByte('\n')
			}
		}
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, io.EOF) {
				break
			}
			return buf.String(), err
		}
	}
	return buf.String(), nil
}

func readUptime(read func(string) ([]byte, error)) string {
	raw, err := read("/proc/uptime")
	if err != nil {
		return "(unavailable)"
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return "(unavailable)"
	}
	return fields[0] + "s"
}

func tailLines(s string, n int) string {
	if n <= 0 || s == "" {
		return s
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n") + "\n"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func itoa(i int) string {
	return fmt.Sprintf("%d", i)
}
