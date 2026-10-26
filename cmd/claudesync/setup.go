package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"github.com/Simplyph0/claudesync/internal/appcfg"
	"github.com/Simplyph0/claudesync/internal/autostart"
	"github.com/Simplyph0/claudesync/internal/client"
	"github.com/Simplyph0/claudesync/internal/vault"
)

// lanURL turns "192.168.1.20" or "nas:9000" into https://host:port.
func lanURL(s string) string {
	s = appcfg.NormalizeURL(s)
	if s == "" {
		return ""
	}
	if u, err := url.Parse(s); err == nil && u.Port() == "" {
		u.Host += ":8443"
		return u.String()
	}
	return s
}

// setupUI is the first-run wizard: connect and sign in, then the passphrase.
func (a *App) setupUI() fyne.CanvasObject {
	c := a.settings()
	name := widget.NewEntry()
	name.SetText(c.DeviceName)
	lan := widget.NewEntry()
	lan.SetPlaceHolder("192.168.1.20:8443")
	lan.SetText(strings.TrimPrefix(c.LANURL, "https://"))
	tun := widget.NewEntry()
	tun.SetPlaceHolder("https://sync.example.com (optional)")
	tun.SetText(c.TunnelURL)
	pw := widget.NewPasswordEntry()
	status := widget.NewLabel("")
	status.Wrapping = fyne.TextWrapWord

	var connect *widget.Button
	connect = widget.NewButton("Connect", func() {
		cfg := a.settings()
		cfg.DeviceName = strings.TrimSpace(name.Text)
		cfg.LANURL = lanURL(lan.Text)
		cfg.TunnelURL = appcfg.NormalizeURL(tun.Text)
		if cfg.DeviceName == "" || (cfg.LANURL == "" && cfg.TunnelURL == "") || pw.Text == "" {
			status.SetText("Enter a name for this PC, at least one server address, and the server password.")
			return
		}
		connect.Disable()
		status.SetText("Connecting…")
		go a.connect(cfg, pw.Text, status, connect)
	})
	connect.Importance = widget.HighImportance

	form := widget.NewForm(
		widget.NewFormItem("This PC's name", name),
		widget.NewFormItem("Server on your network", lan),
		widget.NewFormItem("Cloudflare Tunnel address", tun),
		widget.NewFormItem("Server password", pw),
	)
	intro := widget.NewLabel("Connect this PC to your ClaudeSync server. At home it uses the network address; away from home it uses your Cloudflare Tunnel address.")
	intro.Wrapping = fyne.TextWrapWord
	title := widget.NewLabelWithStyle("Set up ClaudeSync", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	return container.NewPadded(container.NewVBox(title, intro, form, connect, status))
}

func (a *App) connect(cfg appcfg.Settings, password string, status *widget.Label, btn *widget.Button) {
	fail := func(msg string) {
		fyne.Do(func() {
			status.SetText(msg)
			btn.Enable()
		})
	}
	// Step 1: confirm the LAN certificate fingerprint.
	cfg.Pin = ""
	if cfg.LANURL != "" {
		fp, err := client.FetchFingerprint(cfg.LANURL)
		if err != nil {
			if cfg.TunnelURL == "" {
				fail("Couldn't reach " + cfg.LANURL + ": " + err.Error())
				return
			}
			a.log.Printf("LAN address unreachable during setup (%v); using the tunnel. LAN can be confirmed later in Settings.", err)
		} else {
			ok := make(chan bool)
			fyne.Do(func() {
				dialog.ShowConfirm("Check the server's fingerprint",
					"The server on your network identifies itself as:\n\n"+fp+
						"\n\nOn the server, run 'claudesync-server fingerprint'.\nDo the two match?", func(b bool) { ok <- b }, a.win)
			})
			if !<-ok {
				fail("Setup stopped: fingerprint not confirmed.")
				return
			}
			cfg.Pin = fp
		}
	}
	// Step 2: sign in.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cl := client.New(cfg.LANURL, cfg.TunnelURL, cfg.Pin, "")
	resp, err := cl.Login(ctx, password, cfg.DeviceName)
	if err != nil {
		fail("Sign-in failed: " + err.Error())
		return
	}
	cfg.DeviceID = resp.DeviceID
	_, verr := cl.Vault(ctx)
	creating := errors.Is(verr, client.ErrNotFound)
	if verr != nil && !creating {
		fail("Couldn't read the server's passphrase record: " + verr.Error())
		return
	}
	fyne.Do(func() { a.win.SetContent(a.passphraseUI(cfg, cl, resp.Token, creating)) })
}

func (a *App) passphraseUI(cfg appcfg.Settings, cl *client.Client, token string, creating bool) fyne.CanvasObject {
	p1 := widget.NewPasswordEntry()
	p2 := widget.NewPasswordEntry()
	status := widget.NewLabel("")
	status.Wrapping = fyne.TextWrapWord
	var text string
	items := []*widget.FormItem{widget.NewFormItem("Sync passphrase", p1)}
	if creating {
		text = "Choose a sync passphrase. Your chats are encrypted with it on this PC before they're uploaded, so the server can't read them.\n\n" +
			"You'll type it once on each PC. Write it down: if it's lost, the backups can't be decrypted, and it can't be reset."
		items = append(items, widget.NewFormItem("Repeat passphrase", p2))
	} else {
		text = "Enter the sync passphrase you chose on your first PC."
	}
	intro := widget.NewLabel(text)
	intro.Wrapping = fyne.TextWrapWord
	var finish *widget.Button
	finish = widget.NewButton("Finish", func() {
		if creating && (len(p1.Text) < 10 || p1.Text != p2.Text) {
			status.SetText("The passphrase must be at least 10 characters and both entries must match.")
			return
		}
		finish.Disable()
		status.SetText("Working…")
		go func() {
			key, err := a.unlockOrCreate(cl, p1.Text, creating)
			if err != nil {
				fyne.Do(func() {
					status.SetText(err.Error())
					finish.Enable()
				})
				return
			}
			a.finishSetup(cfg, token, key)
		}()
	})
	finish.Importance = widget.HighImportance
	return container.NewPadded(container.NewVBox(
		widget.NewLabelWithStyle("Sync passphrase", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		intro, widget.NewForm(items...), finish, status))
}

func (a *App) unlockOrCreate(cl *client.Client, pass string, creating bool) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if creating {
		v, key, err := vault.New(pass)
		if err != nil {
			return nil, err
		}
		err = cl.SetVault(ctx, v)
		if err == nil {
			return key, nil
		}
		if !errors.Is(err, client.ErrConflict) {
			return nil, err
		}
		// another PC created it in the meantime: fall through and check
	}
	v, err := cl.Vault(ctx)
	if err != nil {
		return nil, err
	}
	key, err := vault.Unlock(v, pass)
	if errors.Is(err, vault.ErrWrongPassphrase) {
		return nil, errors.New("That passphrase doesn't match the one set on your first PC.")
	}
	return key, err
}

func (a *App) finishSetup(cfg appcfg.Settings, token string, key []byte) {
	a.mu.Lock()
	a.cfg = cfg
	a.sec = appcfg.Secrets{Token: token, Key: key}
	a.mu.Unlock()
	if err := appcfg.SaveSecrets(a.sec); err != nil {
		a.log.Printf("saving sign-in: %v", err)
	}
	if err := appcfg.Save(cfg); err != nil {
		a.log.Printf("saving settings: %v", err)
	}
	if err := autostart.Set(cfg.StartAtLogin); err != nil {
		a.log.Printf("start at login: %v", err)
	}
	if err := a.startEngine(); err != nil {
		fyne.Do(func() { dialog.ShowError(err, a.win) })
		return
	}
	fyne.Do(func() {
		a.win.SetContent(a.mainUI())
		dialog.ShowInformation("All set", fmt.Sprintf("%s is connected. ClaudeSync keeps running in the tray: it backs up every %d minutes and checks for sessions from your other PCs every %d minutes.",
			cfg.DeviceName, cfg.BackupMinutes, cfg.RestoreMinutes), a.win)
	})
}
