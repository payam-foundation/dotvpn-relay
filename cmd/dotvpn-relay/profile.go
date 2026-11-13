package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/payam-foundation/dotvpn-relay/internal/config"
	"github.com/payam-foundation/dotvpn-wire/auth"
)

type relayProfile struct {
	RelayID  string `json:"relay_id"`
	IsPublic bool   `json:"is_public"`
	IsSystem bool   `json:"is_system"`
	Status   string `json:"status"`
	Email    string `json:"email"`
	Country  string `json:"country"`
}

func cmdProfile(ctx context.Context, args []string) error {
	fs, cfg := flags("profile")
	public := fs.Bool("public", false, "list this community relay in the public catalog")
	private := fs.Bool("private", false, "hide this community relay from the public catalog")
	email := fs.String("email", "", "notification email (empty clears it)")
	country := fs.String("country", "", "ISO alpha-2 country code (empty clears it)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *public && *private {
		return errors.New("-public and -private conflict")
	}
	var isPublic *bool
	if *public || *private {
		v := *public
		isPublic = &v
	}
	var emailValue, countryValue *string
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "email":
			emailValue = email
		case "country":
			countryValue = country
		}
	})
	c, err := config.Load(configPath(*cfg))
	if err != nil {
		return err
	}
	out, err := updateRelayProfile(ctx, c, isPublic, emailValue, countryValue)
	if err != nil {
		return err
	}
	printRelayProfile(out)
	return nil
}

func cmdDonate(ctx context.Context, args []string) error {
	return cmdPublicToggle(ctx, args, true, "donate")
}

func cmdUndonate(ctx context.Context, args []string) error {
	return cmdPublicToggle(ctx, args, false, "undonate")
}

func cmdPublicToggle(ctx context.Context, args []string, public bool, name string) error {
	fs, cfg := flags(name)
	email := fs.String("email", "", "notification email (empty clears it)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var emailValue *string
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "email" {
			emailValue = email
		}
	})
	c, err := config.Load(configPath(*cfg))
	if err != nil {
		return err
	}
	out, err := updateRelayProfile(ctx, c, &public, emailValue, nil)
	if err != nil {
		return err
	}
	printRelayProfile(out)
	return nil
}

func updateRelayProfile(ctx context.Context, c *config.Config, isPublic *bool, email, country *string) (relayProfile, error) {
	if c.RelayID == "" {
		return relayProfile{}, errors.New("profile requires enroll or register first")
	}
	base, err := resolveControlURL(c.ControlURL)
	if err != nil {
		return relayProfile{}, fmt.Errorf("control_url: %w", err)
	}
	priv, err := auth.DecodePrivateKey(c.IdentityKey)
	if err != nil {
		return relayProfile{}, errors.New("identity_key invalid: run `" + prog() + " init`")
	}
	pub := auth.EncodeKey(priv.Public().(ed25519.PublicKey))
	ts := time.Now().Unix()
	publicPart := "-"
	if isPublic != nil {
		publicPart = "0"
		if *isPublic {
			publicPart = "1"
		}
	}
	emailPart, countryPart := "-", "-"
	if email != nil {
		emailPart = *email
	}
	if country != nil {
		countryPart = *country
	}
	if strings.ContainsAny(emailPart, "|\r\n") || strings.ContainsAny(countryPart, "|\r\n") {
		return relayProfile{}, errors.New("profile values must not contain pipes or newlines")
	}
	msg := []byte(fmt.Sprintf("profile|%s|%s|%s|%s|%d", c.RelayID, publicPart, emailPart, countryPart, ts))
	body, _ := json.Marshal(struct {
		RelayID   string  `json:"relay_id"`
		PublicKey string  `json:"public_key"`
		Public    *bool   `json:"public,omitempty"`
		Email     *string `json:"email,omitempty"`
		Country   *string `json:"country,omitempty"`
		Timestamp int64   `json:"timestamp"`
		Signature string  `json:"signature"`
	}{
		RelayID: c.RelayID, PublicKey: pub, Public: isPublic, Email: email, Country: country,
		Timestamp: ts, Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, msg)),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base.JoinPath("/v1/relays/profile").String(), bytes.NewReader(body))
	if err != nil {
		return relayProfile{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := controlClient(c.ControlPin).Do(req)
	if err != nil {
		return relayProfile{}, errors.New("control unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		var out struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &out)
		if out.Error == "" {
			out.Error = http.StatusText(resp.StatusCode)
		}
		return relayProfile{}, fmt.Errorf("profile refused (%d): %s", resp.StatusCode, out.Error)
	}
	var out relayProfile
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil || out.RelayID != c.RelayID {
		return relayProfile{}, errors.New("profile response invalid")
	}
	return out, nil
}

func printRelayProfile(p relayProfile) {
	fmt.Println("relay_id:", p.RelayID)
	fmt.Println("public:", p.IsPublic)
	fmt.Println("system:", p.IsSystem)
	if p.Status != "" {
		fmt.Println("status:", p.Status)
	}
	if p.Email == "" {
		fmt.Println("email: (not set)")
	} else {
		fmt.Println("email:", p.Email)
	}
	if p.Country != "" {
		fmt.Println("country:", p.Country)
	}
}
