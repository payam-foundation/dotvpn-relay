package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/payam-foundation/dotvpn-relay/internal/config"
)

func cmdBeat(ctx context.Context, args []string) error {
	_ = ctx
	if len(args) < 1 {
		return errors.New("usage: " + prog() + " beat disabled|normal|conservative|telemetry|status|control [-url URL] [-pin PIN]")
	}
	sub, rest := args[0], args[1:]
	switch strings.ToLower(sub) {
	case config.BeatDisabled, config.BeatNormal, config.BeatConservative, config.BeatTelemetry:
		return beatSetMode(sub, rest)
	case "enable":
		// Legacy alias → normal.
		return beatSetMode(config.BeatNormal, rest)
	case "disable":
		return beatSetMode(config.BeatDisabled, rest)
	case "status", "show":
		return beatStatus(rest)
	case "control":
		return beatControl(rest)
	default:
		return errors.New("usage: " + prog() + " beat disabled|normal|conservative|telemetry|status|control [-url URL] [-pin PIN]")
	}
}

func beatSetMode(mode string, args []string) error {
	mode = config.NormalizeBeatMode(mode)
	if err := config.ValidateBeatMode(mode); err != nil {
		return err
	}
	fs, cfg := flags("beat " + mode)
	ctrl := fs.String("control", "", "optional control URL override (https; empty keeps existing / default)")
	pin := fs.String("pin", "", "base64 SHA-256 SPKI pin of control's TLS key (self-signed only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := configPath(*cfg)
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	if c.RelayID == "" && mode != config.BeatDisabled {
		return errors.New("relay_id missing; enroll or register first")
	}
	if mode != config.BeatDisabled && !config.PoolLive(mode) {
		fmt.Println("warning: mode", mode, "will not keep requires_beat / community relays in pools (core stale window is 90s)")
	}
	c.BeatMode = mode
	if *ctrl != "" {
		u, err := parseControlURL(*ctrl)
		if err != nil {
			return err
		}
		c.ControlURL = u.String()
	}
	if *pin != "" {
		c.ControlPin = *pin
	}
	if err := config.Save(path, c); err != nil {
		return err
	}
	resolved, _ := resolveControlURL(c.ControlURL)
	fmt.Println("beat_mode:", c.BeatMode)
	fmt.Println("control:  ", resolved.String(), "(override="+boolStr(c.ControlURL != "")+")")
	if mode != config.BeatDisabled {
		base, jitter, _ := config.BeatInterval(mode)
		fmt.Printf("interval: ~%s (+0–%s jitter)\n", base, jitter)
	}
	fmt.Println("restart the unit for changes to apply:", prog()+".service")
	return nil
}

func beatStatus(args []string) error {
	fs, cfg := flags("beat status")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := configPath(*cfg)
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	resolved, err := resolveControlURL(c.ControlURL)
	if err != nil {
		return err
	}
	fmt.Println("beat_mode:", c.BeatMode)
	fmt.Println("control:  ", resolved.String(), "(override="+boolStr(c.ControlURL != "")+")")
	if base, jitter, ok := config.BeatInterval(c.BeatMode); ok {
		fmt.Printf("interval: ~%s (+0–%s jitter)\n", base, jitter)
		fmt.Println("pool_live:", config.PoolLive(c.BeatMode))
	}
	return nil
}

func beatControl(args []string) error {
	fs, cfg := flags("beat control")
	urlFlag := fs.String("url", "", "control public API URL override (empty clears override → default)")
	clear := fs.Bool("clear", false, "clear control_url override")
	pin := fs.String("pin", "", "base64 SHA-256 SPKI pin (empty leaves unchanged; use -clear-pin to remove)")
	clearPin := fs.Bool("clear-pin", false, "clear control_pin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := configPath(*cfg)
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	if *clear {
		c.ControlURL = ""
		c.ControlPin = "" // pin is bound to a specific control cert
	} else if *urlFlag != "" {
		u, err := parseControlURL(*urlFlag)
		if err != nil {
			return err
		}
		if c.ControlURL != "" && c.ControlURL != u.String() {
			c.ControlPin = "" // URL changed; old pin must not apply
		}
		c.ControlURL = u.String()
	}
	if *clearPin {
		c.ControlPin = ""
	} else if *pin != "" {
		c.ControlPin = *pin
	}
	if err := config.Save(path, c); err != nil {
		return err
	}
	resolved, _ := resolveControlURL(c.ControlURL)
	fmt.Println("control:", resolved.String(), "(override="+boolStr(c.ControlURL != "")+")")
	return nil
}

func boolStr(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}
