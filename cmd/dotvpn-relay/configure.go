package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/payam-foundation/dotvpn-relay/internal/config"
)

func cmdConfigure(ctx context.Context, args []string) error {
	_ = ctx
	fs, cfg := flags("configure")
	beatMode := fs.String("beat-mode", "", "beat mode: disabled|normal|conservative|telemetry")
	ctrl := fs.String("control", "", "persist control_url override (empty leaves unchanged)")
	clearCtrl := fs.Bool("clear-control", false, "clear control_url override")
	pin := fs.String("pin", "", "control TLS SPKI pin")
	inMbps := fs.Float64("bandwidth-ingress", -1, "ingress Mbps cap (0=unlimited; omit to leave)")
	outMbps := fs.Float64("bandwidth-egress", -1, "egress Mbps cap (0=unlimited; omit to leave)")
	avgWindow := fs.String("avg-window", "", "rolling average window (e.g. 24h); empty clears avg soft-cap when -clear-avg")
	avgCap := fs.Float64("avg-cap", -1, "rolling average cap Mbps")
	throttle := fs.Float64("throttle", -1, "Mbps when average soft-cap engages")
	clearAvg := fs.Bool("clear-avg", false, "clear rolling-average soft-cap fields")
	domain := fs.String("domain", "", "local domain (TLS SNI / carrier zone; does not PATCH control catalog)")
	clearDomain := fs.Bool("clear-domain", false, "clear domain")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := configPath(*cfg)
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	changed := false
	if *beatMode != "" {
		if err := config.ValidateBeatMode(*beatMode); err != nil {
			return err
		}
		c.BeatMode = config.NormalizeBeatMode(*beatMode)
		changed = true
	}
	if *clearCtrl {
		c.ControlURL = ""
		changed = true
	} else if *ctrl != "" {
		u, err := parseControlURL(*ctrl)
		if err != nil {
			return err
		}
		c.ControlURL = u.String()
		changed = true
	}
	if *pin != "" {
		c.ControlPin = *pin
		changed = true
	}
	if *inMbps >= 0 {
		c.BandwidthIngressMbps = *inMbps
		changed = true
	}
	if *outMbps >= 0 {
		c.BandwidthEgressMbps = *outMbps
		changed = true
	}
	if *clearAvg {
		c.BandwidthAvgWindow = ""
		c.BandwidthAvgCapMbps = 0
		c.BandwidthThrottleMbps = 0
		changed = true
	} else if *avgWindow != "" || *avgCap >= 0 || *throttle >= 0 {
		if *avgWindow != "" {
			c.BandwidthAvgWindow = *avgWindow
		}
		if *avgCap >= 0 {
			c.BandwidthAvgCapMbps = *avgCap
		}
		if *throttle >= 0 {
			c.BandwidthThrottleMbps = *throttle
		}
		changed = true
	}
	if *clearDomain {
		c.Domain = ""
		changed = true
	} else if *domain != "" {
		c.Domain = *domain
		changed = true
	}
	if !changed {
		return errors.New("configure: pass at least one flag to change")
	}
	if err := c.Validate(); err != nil {
		return err
	}
	if err := config.Save(path, c); err != nil {
		return err
	}
	fmt.Println("updated", path)
	fmt.Println("beat_mode:", c.BeatMode)
	resolved, _ := resolveControlURL(c.ControlURL)
	fmt.Println("control:  ", resolved.String(), "(override="+boolStr(c.ControlURL != "")+")")
	fmt.Printf("bandwidth: in=%.2f out=%.2f Mbps\n", c.BandwidthIngressMbps, c.BandwidthEgressMbps)
	if c.BandwidthAvgWindow != "" {
		fmt.Printf("avg soft:  window=%s cap=%.2f throttle=%.2f\n",
			c.BandwidthAvgWindow, c.BandwidthAvgCapMbps, c.BandwidthThrottleMbps)
	}
	if c.Domain != "" {
		fmt.Println("domain:", c.Domain)
	}
	if c.DNSListen != "" {
		fmt.Println("dns_listen:", c.DNSListen)
	}
	return nil
}
