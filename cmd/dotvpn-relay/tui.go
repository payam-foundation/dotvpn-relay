package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/payam-foundation/dotvpn-relay/internal/config"
	"github.com/payam-foundation/dotvpn-relay/internal/domaincheck"
)

func cmdTUI(ctx context.Context, args []string) error {
	fs, cfg := flags("tui")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := configPath(*cfg)
	if !isTTY(os.Stdin) || !isTTY(os.Stdout) {
		return cmdStatus(ctx, []string{"-config", path})
	}
	return runTUI(ctx, path)
}

func runTUI(ctx context.Context, path string) error {
	in := bufio.NewReader(os.Stdin)
	historyIn := make([]float64, 0, 60)
	historyOut := make([]float64, 0, 60)
	var lastIn, lastOut uint64
	var lastAt time.Time

	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		c, err := config.Load(path)
		if err != nil {
			return err
		}
		clearScreen()
		fmt.Printf("%s ops  %s\n", prog(), version)
		fmt.Println(strings.Repeat("─", 48))
		fmt.Printf("config   %s\n", path)
		fmt.Printf("listen   %s\n", c.Listen)
		fmt.Printf("unit     %s\n", unitState(ctx, prog()+".service"))
		if c.RelayID != "" {
			fmt.Printf("relay    %s\n", c.RelayID)
		}
		resolved, _ := resolveControlURL(c.ControlURL)
		fmt.Printf("control  %s (override=%s)\n", resolved.String(), boolStr(c.ControlURL != ""))
		fmt.Printf("beat     %s", c.BeatMode)
		if base, jitter, ok := config.BeatInterval(c.BeatMode); ok {
			fmt.Printf("  (~%s +0–%s)", base, jitter)
			if !config.PoolLive(c.BeatMode) {
				fmt.Printf("  [telemetry-only]")
			}
		}
		fmt.Println()
		fmt.Printf("bw caps  in=%.2f Mbps  out=%.2f Mbps\n", c.BandwidthIngressMbps, c.BandwidthEgressMbps)
		if c.BandwidthAvgWindow != "" {
			fmt.Printf("bw avg   window=%s cap=%.2f throttle=%.2f\n",
				c.BandwidthAvgWindow, c.BandwidthAvgCapMbps, c.BandwidthThrottleMbps)
		}
		fmt.Println(strings.Repeat("─", 48))
		if domain := c.CheckDomain(); domain == "" {
			fmt.Println("domain   (unset)")
		} else {
			fmt.Printf("domain   %s\n", domain)
			if c.DNSListen != "" {
				fmt.Printf("dns      carrier on %s\n", c.DNSListen)
			}
			dctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			rep := domaincheck.Check(dctx, domain, c.Listen, c.DNSListen)
			cancel()
			for _, r := range rep.Results {
				mark := "ok"
				if r.Skipped {
					mark = "skip"
				} else if !r.OK {
					mark = "FAIL"
				}
				fmt.Printf("  %-5s %s: %s\n", mark, r.Name, r.Detail)
			}
		}

		parked, active, bytesIn, bytesOut, sessions, herr := fetchMetrics(ctx, c.AdminListen)
		inBps, outBps, throttled := fetchBandwidthMetrics(ctx, c.AdminListen)
		fmt.Println(strings.Repeat("─", 48))
		if herr != nil {
			fmt.Printf("metrics  unavailable (%v)\n", herr)
		} else {
			fmt.Printf("parked   %d    active %d    sessions %d\n", parked, active, sessions)
			fmt.Printf("bytes    in=%d  out=%d\n", bytesIn, bytesOut)
			now := time.Now()
			if !lastAt.IsZero() {
				sec := now.Sub(lastAt).Seconds()
				if sec > 0 {
					rin := float64(uint64(bytesIn)-lastIn) * 8 / sec / 1e6
					rout := float64(uint64(bytesOut)-lastOut) * 8 / sec / 1e6
					if inBps == 0 {
						inBps = uint64(rin * 1e6 / 8)
					}
					if outBps == 0 {
						outBps = uint64(rout * 1e6 / 8)
					}
					historyIn = appendMbps(historyIn, rin)
					historyOut = appendMbps(historyOut, rout)
				}
			}
			lastIn, lastOut, lastAt = uint64(bytesIn), uint64(bytesOut), now
			fmt.Printf("rate     in=%.2f Mbps  out=%.2f Mbps", float64(inBps)*8/1e6, float64(outBps)*8/1e6)
			if throttled {
				fmt.Printf("  THROTTLED")
			}
			fmt.Println()
			fmt.Printf("chart in  %s\n", sparkline(historyIn))
			fmt.Printf("chart out %s\n", sparkline(historyOut))
		}
		fmt.Println(strings.Repeat("─", 48))
		fmt.Println("[1] beat mode   [2] control URL   [3] bandwidth")
		fmt.Println("[4] refresh     [5] plain status  [q] quit")
		fmt.Print("> ")

		lineCh := make(chan string, 1)
		go func() {
			s, _ := in.ReadString('\n')
			lineCh <- s
		}()
		var line string
		select {
		case <-ctx.Done():
			return nil
		case line = <-lineCh:
		}
		switch strings.TrimSpace(strings.ToLower(line)) {
		case "q", "quit", "exit":
			return nil
		case "1":
			if err := tuiSetBeatMode(path, in); err != nil {
				fmt.Println(err)
				pause(in)
			}
		case "2":
			if err := tuiSetControl(path, in); err != nil {
				fmt.Println(err)
				pause(in)
			}
		case "3":
			if err := tuiSetBandwidth(path, in); err != nil {
				fmt.Println(err)
				pause(in)
			}
		case "5":
			_ = cmdStatus(ctx, []string{"-config", path})
			pause(in)
		case "4", "":
			// refresh
		default:
			fmt.Println("unknown key")
			time.Sleep(800 * time.Millisecond)
		}
	}
}

func clearScreen() {
	fmt.Print("\033[H\033[2J")
}

func appendMbps(h []float64, v float64) []float64 {
	h = append(h, v)
	if len(h) > 48 {
		h = h[len(h)-48:]
	}
	return h
}

func sparkline(values []float64) string {
	if len(values) == 0 {
		return "(collecting…)"
	}
	blocks := []rune("▁▂▃▄▅▆▇█")
	max := 0.0
	for _, v := range values {
		if v > max {
			max = v
		}
	}
	if max <= 0 {
		return strings.Repeat(string(blocks[0]), len(values))
	}
	var b strings.Builder
	for _, v := range values {
		i := int(v / max * float64(len(blocks)-1))
		if i < 0 {
			i = 0
		}
		if i >= len(blocks) {
			i = len(blocks) - 1
		}
		b.WriteRune(blocks[i])
	}
	return b.String()
}

func pause(in *bufio.Reader) {
	fmt.Print("press enter…")
	_, _ = in.ReadString('\n')
}

func tuiSetBeatMode(path string, in *bufio.Reader) error {
	fmt.Println("modes: disabled | normal | conservative | telemetry")
	fmt.Print("mode> ")
	line, err := in.ReadString('\n')
	if err != nil {
		return err
	}
	mode := config.NormalizeBeatMode(strings.TrimSpace(line))
	if err := config.ValidateBeatMode(mode); err != nil {
		return err
	}
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	c.BeatMode = mode
	if err := config.Save(path, c); err != nil {
		return err
	}
	fmt.Println("saved beat_mode=", mode, "— restart", prog()+".service")
	pause(in)
	return nil
}

func tuiSetControl(path string, in *bufio.Reader) error {
	fmt.Print("control URL (empty=clear override)> ")
	line, err := in.ReadString('\n')
	if err != nil {
		return err
	}
	raw := strings.TrimSpace(line)
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	if raw == "" {
		c.ControlURL = ""
	} else {
		u, err := parseControlURL(raw)
		if err != nil {
			return err
		}
		c.ControlURL = u.String()
	}
	if err := config.Save(path, c); err != nil {
		return err
	}
	resolved, _ := resolveControlURL(c.ControlURL)
	fmt.Println("control=", resolved.String())
	pause(in)
	return nil
}

func tuiSetBandwidth(path string, in *bufio.Reader) error {
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	fmt.Print("ingress Mbps (0=unlimited)> ")
	inLine, err := in.ReadString('\n')
	if err != nil {
		return err
	}
	fmt.Print("egress Mbps (0=unlimited)> ")
	outLine, err := in.ReadString('\n')
	if err != nil {
		return err
	}
	inMbps, err := strconv.ParseFloat(strings.TrimSpace(inLine), 64)
	if err != nil || inMbps < 0 {
		return fmt.Errorf("invalid ingress")
	}
	outMbps, err := strconv.ParseFloat(strings.TrimSpace(outLine), 64)
	if err != nil || outMbps < 0 {
		return fmt.Errorf("invalid egress")
	}
	c.BandwidthIngressMbps = inMbps
	c.BandwidthEgressMbps = outMbps
	fmt.Print("24h avg soft-cap? [y/N]> ")
	avgLine, _ := in.ReadString('\n')
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(avgLine)), "y") {
		fmt.Print("avg cap Mbps> ")
		capLine, _ := in.ReadString('\n')
		fmt.Print("throttle Mbps> ")
		thLine, _ := in.ReadString('\n')
		capMbps, err1 := strconv.ParseFloat(strings.TrimSpace(capLine), 64)
		thMbps, err2 := strconv.ParseFloat(strings.TrimSpace(thLine), 64)
		if err1 != nil || err2 != nil || capMbps <= 0 || thMbps <= 0 {
			return fmt.Errorf("invalid avg/throttle")
		}
		c.BandwidthAvgWindow = "24h"
		c.BandwidthAvgCapMbps = capMbps
		c.BandwidthThrottleMbps = thMbps
	} else {
		c.BandwidthAvgWindow = ""
		c.BandwidthAvgCapMbps = 0
		c.BandwidthThrottleMbps = 0
	}
	if err := config.Save(path, c); err != nil {
		return err
	}
	fmt.Println("saved — restart", prog()+".service")
	pause(in)
	return nil
}

func fetchBandwidthMetrics(ctx context.Context, addr string) (inBps, outBps uint64, throttled bool) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, adminURL(addr, "/metrics"), nil)
	if err != nil {
		return 0, 0, false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, 0, false
	}
	defer resp.Body.Close()
	return parseBandwidthMetrics(resp.Body)
}

func parseBandwidthMetrics(r io.Reader) (inBps, outBps uint64, throttled bool) {
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
		n, err := strconv.ParseUint(val, 10, 64)
		if err != nil {
			continue
		}
		switch {
		case strings.HasSuffix(name, "_ingress_bps"):
			inBps = n
		case strings.HasSuffix(name, "_egress_bps"):
			outBps = n
		case strings.HasSuffix(name, "_bandwidth_throttled"):
			throttled = n != 0
		}
	}
	return inBps, outBps, throttled
}
