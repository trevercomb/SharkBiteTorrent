package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sharkbite/internal/secret"
	"sharkbite/internal/vpnprov"
)

// VPN provider accounts, stored encrypted in accounts.json (one per provider).

func (a *App) accountsPath() string { return filepath.Join(a.configDir, "accounts.json") }

// loadAccounts returns the stored accounts. An error means accounts.json
// exists but couldn't be read (keyring locked, damaged), so it mustn't be
// saved over.
func (a *App) loadAccounts() (map[string]*vpnprov.Account, error) {
	accs := map[string]*vpnprov.Account{}
	data, err := secret.ReadFile(a.accountsPath())
	if errors.Is(err, os.ErrNotExist) {
		return accs, nil
	}
	if err == nil {
		err = json.Unmarshal(data, &accs)
	}
	if err != nil {
		return accs, fmt.Errorf("couldn't read the saved VPN sign-ins (%v)", err)
	}
	return accs, nil
}

func (a *App) saveAccounts(accs map[string]*vpnprov.Account) error {
	data, _ := json.Marshal(accs)
	return secret.WriteFile(a.accountsPath(), data)
}

// saveAccountCache saves the cached state (a session token) Connect left in
// acc, if that's still the stored account for provider (not signed out or
// replaced meanwhile).
func (a *App) saveAccountCache(provider string, acc *vpnprov.Account) {
	a.accountsMu.Lock()
	defer a.accountsMu.Unlock()
	accs, err := a.loadAccounts()
	cur := accs[provider]
	if err != nil || cur == nil || accountKey(cur) != accountKey(acc) {
		return
	}
	changed := false
	for k, v := range acc.Data {
		if strings.HasPrefix(k, vpnprov.CachePrefix) && cur.Data[k] != v {
			cur.Data[k] = v
			changed = true
		}
	}
	if changed {
		a.saveAccounts(accs)
	}
}

// rateLimitError is shown while a provider is turning requests away.
type rateLimitError struct {
	name  string
	until time.Time
}

func (e *rateLimitError) Error() string {
	return fmt.Sprintf("%s is turning sign-ins away for now: too many in a short time (HTTP 429). "+
		"The app will try again by itself at %s.", e.name, e.until.Format("3:04 PM"))
}

// The wait before trying again when the provider doesn't say how long:
// 10 minutes, doubling each time it's still limited, up to an hour.
const (
	limitFirstWait = 10 * time.Minute
	limitMaxWait   = time.Hour
)

func providerName(provider string) string {
	if p, ok := vpnprov.Get(provider); ok {
		return p.Info().Name
	}
	return provider
}

// rateLimitedNow returns a *rateLimitError if provider is still turning
// requests away.
func (a *App) rateLimitedNow(provider string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.limitProvider != provider || !time.Now().Before(a.limitUntil) {
		return nil
	}
	return &rateLimitError{providerName(provider), a.limitUntil}
}

// noteRateLimit returns err, or if it says provider is limiting requests,
// records that, schedules connecting again once the wait is over, and
// returns the message to show.
func (a *App) noteRateLimit(provider string, err error) error {
	var rl *vpnprov.RateLimitError
	if !errors.As(err, &rl) {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.limitProvider != provider {
		a.limitStrikes = 0
	}
	wait := rl.RetryAfter
	if wait <= 0 {
		wait = limitFirstWait << min(a.limitStrikes, 3)
	}
	wait = min(max(wait, time.Minute), limitMaxWait)
	a.limitProvider, a.limitUntil = provider, time.Now().Add(wait)
	a.limitStrikes++
	if a.limitTimer != nil {
		a.limitTimer.Stop()
	}
	a.limitTimer = time.AfterFunc(wait, func() {
		a.mu.Lock()
		retry := a.vpn.State == "error" && a.settings.VpnProvider == "account" &&
			a.settings.AccountProvider == provider && !a.shuttingDown
		a.mu.Unlock()
		if retry {
			a.Reconnect()
		}
	})
	return &rateLimitError{providerName(provider), a.limitUntil}
}

// clearRateLimit forgets a limit once provider accepted a connection.
func (a *App) clearRateLimit(provider string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.limitProvider == provider {
		a.limitProvider, a.limitStrikes = "", 0
		if a.limitTimer != nil {
			a.limitTimer.Stop()
			a.limitTimer = nil
		}
	}
}

func (a *App) account(provider string) *vpnprov.Account {
	accs, _ := a.loadAccounts()
	return accs[provider]
}

// --- Bound to the frontend ---

func (a *App) ListVpnProviders() []vpnprov.Info { return vpnprov.All() }

func (a *App) GetVpnRegions(provider string) ([]vpnprov.Region, error) {
	p, ok := vpnprov.Get(provider)
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", provider)
	}
	// With no VPN connected this request would go out on the real
	// connection. Only do that for a provider the user signed in to (they're
	// about to connect to it, which shows it their IP anyway), not for every
	// provider they click on while browsing Settings.
	a.mu.Lock()
	noTunnel, mode := a.tun == nil, a.settings.VpnProvider
	a.mu.Unlock()
	mayGoDirect := mode == "none" || a.account(provider) != nil
	if noTunnel && !mayGoDirect {
		return []vpnprov.Region{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if !mayGoDirect {
		// Decided at connect time, so a VPN that drops meanwhile makes the
		// request fail instead of going out directly.
		ctx = vpnprov.TunnelOnly(ctx)
	}
	return p.Regions(ctx)
}

type LoginResult struct {
	Display string `json:"display"` // set on success
	// NeedField is set when the provider needs one more input (2FA code,
	// confirmation); show it and call LoginVpnAccount again.
	NeedField *vpnprov.Field `json:"needField"`
	Message   string         `json:"message"`
}

func (a *App) LoginVpnAccount(provider string, creds map[string]string) (LoginResult, error) {
	p, ok := vpnprov.Get(provider)
	if !ok {
		return LoginResult{}, fmt.Errorf("unknown provider %q", provider)
	}
	a.accountsMu.Lock()
	defer a.accountsMu.Unlock()
	// Checked before signing in, so a failure can't leave a new device
	// registered at the provider and forgotten here.
	accs, err := a.loadAccounts()
	if err != nil {
		return LoginResult{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	acc, err := p.Login(ctx, creds)
	var need *vpnprov.NeedFieldError
	if errors.As(err, &need) {
		return LoginResult{NeedField: &need.Field, Message: need.Message}, nil
	}
	if err != nil {
		return LoginResult{}, err
	}
	if old := accs[provider]; old != nil {
		p.Logout(ctx, old) // free the previous device/session slot
	}
	accs[provider] = acc
	if err := a.saveAccounts(accs); err != nil {
		return LoginResult{}, err
	}
	return LoginResult{Display: acc.Display}, nil
}

// forgetAccount signs out of provider's account and removes it from
// accounts.json.
func (a *App) forgetAccount(provider string) error {
	a.accountsMu.Lock()
	defer a.accountsMu.Unlock()
	accs, err := a.loadAccounts()
	if err != nil {
		return err
	}
	acc := accs[provider]
	if acc == nil {
		return nil
	}
	if p, ok := vpnprov.Get(provider); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		p.Logout(ctx, acc)
		cancel()
	}
	delete(accs, provider)
	if len(accs) == 0 {
		err = os.Remove(a.accountsPath())
	} else {
		err = a.saveAccounts(accs)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// LogoutVpnAccount signs out and forgets the stored credentials. If that
// account was in use, the app falls back to WARP.
func (a *App) LogoutVpnAccount(provider string) error {
	if err := a.forgetAccount(provider); err != nil {
		return err
	}
	a.mu.Lock()
	inUse := a.settings.VpnProvider == "account" && a.settings.AccountProvider == provider
	s := a.settings
	a.mu.Unlock()
	if inUse {
		s.VpnProvider = "warp"
		return a.SaveSettings(s)
	}
	return nil
}
