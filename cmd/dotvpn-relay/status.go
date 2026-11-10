package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/payam-foundation/dotvpn-relay/internal/config"
)

func cmdStatus(ctx context.Context, args []string) error {
	fs, cfg := flags("status")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := configPath(*cfg)
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	unit := prog() + ".service"
	fmt.Printf("config:  %s\n", path)
	fmt.Printf("listen:  %s\n", c.Listen)
	fmt.Printf("admin:   %s\n", c.AdminListen)
	if c.DNSZone != "" {
		fmt.Printf("dns:     %s on %s\n", c.DNSZone, c.DNSListen)
	} else {
		fmt.Printf("dns:     off\n")
	}
	if c.Decoy != "" {
		fmt.Printf("decoy:   %s\n", c.Decoy)
	}
	if c.RelayID != "" {
		fmt.Printf("relay:   %s\n", c.RelayID)
	}
	fmt.Printf("unit:    %s\n", unitState(ctx, unit))
	parked, active, herr := fetchMetrics(ctx, c.AdminListen)
	if herr != nil {
		fmt.Printf("health:  %v\n", herr)
		return nil
	}
	fmt.Printf("health:  ok\n")
	fmt.Printf("parked:  %d\n", parked)
	fmt.Printf("active:  %d\n", active)
	return nil
}

func cmdDoctor(ctx context.Context, args []string) error {
	fs, cfg := flags("doctor")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := configPath(*cfg)
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	var failed int
	check := func(ok bool, name, detail string) {
		mark := "ok"
		if !ok {
			mark = "FAIL"
			failed++
		}
		fmt.Printf("  %-5s %s: %s\n", mark, name, detail)
	}

	fmt.Println("doctor")
	unit := prog() + ".service"
	st := unitState(ctx, unit)
	check(strings.HasPrefix(st, "active"), "systemd", st)

	if err := pingHealthz(ctx, c.AdminListen); err != nil {
		check(false, "healthz", err.Error())
	} else {
		check(true, "healthz", "ok at "+c.AdminListen)
	}

	parked, active, err := fetchMetrics(ctx, c.AdminListen)
	if err != nil {
		check(false, "metrics", err.Error())
	} else {
		check(true, "metrics", fmt.Sprintf("parked=%d active=%d", parked, active))
	}

	_, port, err := splitHostPortDefault(c.Listen, "443")
	if err != nil {
		check(false, "listen", err.Error())
	} else if tcpLocalListening(port) {
		check(true, "tcp "+port, "listening ("+c.Listen+")")
	} else {
		check(false, "tcp "+port, "not listening ("+c.Listen+")")
	}

	if c.DNSListen != "" {
		_, dnsPort, err := splitHostPortDefault(c.DNSListen, "53")
		if err != nil {
			check(false, "udp dns", err.Error())
		} else if udpLocalListening(dnsPort) {
			check(true, "udp "+dnsPort, "listening ("+c.DNSListen+")")
		} else {
			check(false, "udp "+dnsPort, "not listening ("+c.DNSListen+")")
		}
	}

	if c.Decoy != "" {
		dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := dialTCP(dctx, c.Decoy)
		cancel()
		if err != nil {
			check(false, "decoy", c.Decoy+": "+err.Error())
		} else {
			check(true, "decoy", c.Decoy+" accepts TCP")
		}
	}

	if free, err := rootFreeKB(); err != nil {
		check(false, "disk", err.Error())
	} else if free < 524288 {
		check(false, "disk", fmt.Sprintf("/ has %d kB free (<512 MiB)", free))
	} else {
		check(true, "disk", fmt.Sprintf("/ has %d kB free", free))
	}

	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	return nil
}

func unitState(ctx context.Context, unit string) string {
	cmd := exec.CommandContext(ctx, "systemctl", "is-active", unit)
	out, err := cmd.Output()
	s := strings.TrimSpace(string(out))
	if err != nil && s == "" {
		return "unknown"
	}
	if s == "" {
		return "inactive"
	}
	return s
}

func pingHealthz(ctx context.Context, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, adminURL(addr, "/healthz"), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	return nil
}

func fetchMetrics(ctx context.Context, addr string) (parked, active int, err error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, adminURL(addr, "/metrics"), nil)
	if err != nil {
		return 0, 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("HTTP %s", resp.Status)
	}
	return parseMetrics(resp.Body)
}

func adminURL(addr, path string) string {
	return "http://" + addr + path
}

func parseMetrics(r io.Reader) (parked, active int, err error) {
	parked, active = -1, -1
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name, val := fields[0], fields[1]
		n, perr := strconv.Atoi(val)
		if perr != nil {
			continue
		}
		switch {
		case strings.HasSuffix(name, "_parked"):
			parked = n
		case strings.HasSuffix(name, "_active"):
			active = n
		}
	}
	if err := sc.Err(); err != nil {
		return 0, 0, err
	}
	if parked < 0 {
		return 0, 0, errors.New("parked metric missing")
	}
	if active < 0 {
		active = 0
	}
	return parked, active, nil
}

func splitHostPortDefault(addr, defPort string) (host, port string, err error) {
	if addr == "" {
		return "", "", errors.New("empty address")
	}
	if strings.HasPrefix(addr, ":") {
		p := strings.TrimPrefix(addr, ":")
		if p == "" {
			p = defPort
		}
		return "", p, nil
	}
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", "", err
	}
	if p == "" {
		p = defPort
	}
	return h, p, nil
}

func tcpLocalListening(port string) bool {
	return ssHas("tln", port)
}

func udpLocalListening(port string) bool {
	return ssHas("uln", port)
}

func ssHas(mode, port string) bool {
	out, err := exec.Command("ss", "-"+mode).Output()
	if err != nil {
		return false
	}
	needle := ":" + port
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

func dialTCP(ctx context.Context, addr string) error {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return c.Close()
}

func rootFreeKB() (int64, error) {
	out, err := exec.Command("df", "-Pk", "/").Output()
	if err != nil {
		return 0, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return 0, errors.New("df output short")
	}
	fields := strings.Fields(lines[1])
	if len(fields) < 4 {
		return 0, errors.New("df fields short")
	}
	return strconv.ParseInt(fields[3], 10, 64)
}
