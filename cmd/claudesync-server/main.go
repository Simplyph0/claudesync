// Command claudesync-server is the headless ClaudeSync server. It runs in the
// foreground or as a Windows service / systemd unit.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Simplyph0/claudesync/internal/proto"
	"github.com/Simplyph0/claudesync/internal/server"
	"github.com/kardianos/service"
	"golang.org/x/term"
)

const usage = `claudesync-server %s

Usage: claudesync-server <command> [--data DIR]

Commands:
  init             set the server password and listen addresses (first run)
  run              run in the foreground
  install          install as a background service (run as admin/root), then start it
  uninstall        stop and remove the service
  start | stop | restart | status
  reset-password   set a new password from this machine (if you forgot it)
                   add --logout-all to also sign every PC out
  fingerprint      show the certificate fingerprint PCs are asked to confirm
  devices          list connected PCs

Flags:
  --data DIR        data folder (default %s)
  --lan ADDR        HTTPS listen address for the LAN       (init only, default :8443)
  --local ADDR      plain HTTP address for cloudflared, "" disables (init only, default 127.0.0.1:8480)
  --retention N     versions kept per item                 (init only, default 20)

The password can also be passed in the CLAUDESYNC_PASSWORD environment variable.
`

func defaultDataDir() string {
	if runtime.GOOS == "windows" {
		if pd := os.Getenv("ProgramData"); pd != "" {
			return filepath.Join(pd, "ClaudeSync")
		}
	}
	if os.Geteuid() == 0 {
		return "/var/lib/claudesync"
	}
	if d, err := os.UserHomeDir(); err == nil {
		return filepath.Join(d, ".local", "share", "claudesync")
	}
	return "claudesync-data"
}

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		fmt.Printf(usage, proto.Version, defaultDataDir())
		return
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	dataDir := fs.String("data", defaultDataDir(), "data folder")
	lan := fs.String("lan", ":8443", "LAN HTTPS address")
	local := fs.String("local", "127.0.0.1:8480", "loopback HTTP address for cloudflared")
	retention := fs.Int("retention", server.DefaultRetention, "versions kept per item")
	logoutAll := fs.Bool("logout-all", false, "sign every PC out (reset-password)")
	_ = fs.Parse(os.Args[2:])

	abs, err := filepath.Abs(*dataDir)
	if err == nil {
		*dataDir = abs
	}

	switch cmd {
	case "init":
		exitOn(cmdInit(*dataDir, *lan, *local, *retention))
	case "reset-password":
		exitOn(cmdResetPassword(*dataDir, *logoutAll))
	case "fingerprint":
		exitOn(cmdFingerprint(*dataDir))
	case "devices":
		exitOn(cmdDevices(*dataDir))
	case "run", "install", "uninstall", "start", "stop", "restart", "status":
		exitOn(cmdService(cmd, *dataDir))
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		fmt.Printf(usage, proto.Version, defaultDataDir())
		os.Exit(2)
	}
}

func exitOn(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func readPassword(prompt string) (string, error) {
	if pw := os.Getenv("CLAUDESYNC_PASSWORD"); pw != "" {
		return pw, nil
	}
	fmt.Print(prompt)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		return string(b), err
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}

func askNewPassword() (string, error) {
	pw, err := readPassword("New server password (8+ characters): ")
	if err != nil {
		return "", err
	}
	if os.Getenv("CLAUDESYNC_PASSWORD") == "" {
		again, err := readPassword("Repeat password: ")
		if err != nil {
			return "", err
		}
		if again != pw {
			return "", errors.New("passwords don't match")
		}
	}
	return pw, nil
}

func cmdInit(dir, lan, local string, retention int) error {
	st, err := server.Open(dir)
	if err != nil {
		return err
	}
	if st.HasPassword() {
		return errors.New("server already initialised; use reset-password to change the password")
	}
	pw, err := askNewPassword()
	if err != nil {
		return err
	}
	if err := st.UpdateConfig(func(c *server.Config) {
		c.LANAddr, c.LocalAddr, c.Retention = lan, local, retention
	}); err != nil {
		return err
	}
	if err := st.SetPassword(pw); err != nil {
		return err
	}
	cert, err := server.LoadOrCreateCert(dir)
	if err != nil {
		return err
	}
	fmt.Println("Server initialised in", dir)
	fmt.Println("Certificate fingerprint (PCs will ask you to confirm this):")
	fmt.Println("  " + server.Fingerprint(cert.Certificate[0]))
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		fmt.Println()
		fmt.Println("Note: you ran init without sudo, so this setup is in your home folder.")
		fmt.Println("The background service (installed with sudo) uses /var/lib/claudesync instead.")
		fmt.Println("To run it as a service, run 'sudo claudesync-server init' and then 'sudo claudesync-server install'.")
		return nil
	}
	fmt.Println("Next: 'claudesync-server install' (as admin/root) to run it in the background,")
	fmt.Println("or 'claudesync-server run' to try it in this window.")
	return nil
}

func cmdResetPassword(dir string, logoutAll bool) error {
	st, err := server.Open(dir)
	if err != nil {
		return err
	}
	pw, err := askNewPassword()
	if err != nil {
		return err
	}
	if err := st.SetPassword(pw); err != nil {
		return err
	}
	if logoutAll {
		if err := st.RevokeAll(); err != nil {
			return err
		}
		fmt.Println("Password changed; every PC has been signed out.")
	} else {
		fmt.Println("Password changed. PCs already connected stay connected.")
	}
	return nil
}

func cmdFingerprint(dir string) error {
	cert, err := server.LoadOrCreateCert(dir)
	if err != nil {
		return err
	}
	fmt.Println(server.Fingerprint(cert.Certificate[0]))
	return nil
}

func cmdDevices(dir string) error {
	st, err := server.Open(dir)
	if err != nil {
		return err
	}
	for _, d := range st.Devices() {
		fmt.Printf("%s  %-24s last seen %s\n", d.ID, d.Name, d.LastSeen.Local().Format(time.DateTime))
	}
	return nil
}

// --- service ---

type program struct {
	dir     string
	servers []*http.Server
	cancel  context.CancelFunc
}

func (p *program) Start(s service.Service) error {
	st, err := server.Open(p.dir)
	if err != nil {
		return err
	}
	if !st.HasPassword() {
		return fmt.Errorf("no password set; run 'claudesync-server init --data %q' first", p.dir)
	}
	cert, err := server.LoadOrCreateCert(p.dir)
	if err != nil {
		return err
	}
	h := server.New(st).Handler()
	cfg := st.Config()
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel

	if cfg.LANAddr != "" {
		srv := &http.Server{Addr: cfg.LANAddr, Handler: h, ReadHeaderTimeout: 15 * time.Second,
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
		p.servers = append(p.servers, srv)
		go func() {
			log.Printf("LAN HTTPS listening on %s (fingerprint %s)", cfg.LANAddr, server.Fingerprint(cert.Certificate[0]))
			if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("LAN listener: %v", err)
			}
		}()
	}
	if cfg.LocalAddr != "" {
		srv := &http.Server{Addr: cfg.LocalAddr, Handler: h, ReadHeaderTimeout: 15 * time.Second}
		p.servers = append(p.servers, srv)
		go func() {
			log.Printf("tunnel HTTP listening on %s (point cloudflared at http://%s)", cfg.LocalAddr, cfg.LocalAddr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("tunnel listener: %v", err)
			}
		}()
	}
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				st.CleanUploads()
			}
		}
	}()
	return nil
}

func (p *program) Stop(s service.Service) error {
	if p.cancel != nil {
		p.cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, srv := range p.servers {
		_ = srv.Shutdown(ctx)
	}
	return nil
}

func cmdService(cmd, dir string) error {
	cfg := &service.Config{
		Name:        "ClaudeSyncServer",
		DisplayName: "ClaudeSync Server",
		Description: "Syncs Claude sessions between your PCs.",
		Arguments:   []string{"run", "--data", dir},
	}
	if runtime.GOOS == "linux" {
		cfg.Dependencies = []string{"After=network-online.target", "Wants=network-online.target"}
	}
	prg := &program{dir: dir}
	svc, err := service.New(prg, cfg)
	if err != nil {
		return err
	}
	switch cmd {
	case "run":
		if service.Interactive() {
			log.Printf("ClaudeSync server %s, data in %s (Ctrl+C to stop)", proto.Version, dir)
		} else if f, err := os.OpenFile(filepath.Join(dir, "server.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			log.SetOutput(f)
		}
		return svc.Run()
	case "install":
		// The service would start and immediately fail without a password.
		if st, err := server.Open(dir); err != nil || !st.HasPassword() {
			return fmt.Errorf("no server password in %s yet; run init with the same user first (on Linux: 'sudo claudesync-server init')", dir)
		}
		if err := svc.Install(); err != nil {
			return err
		}
		if err := svc.Start(); err != nil {
			return err
		}
		fmt.Println("Service installed and started. Logs:", filepath.Join(dir, "server.log"))
		return nil
	case "status":
		s, err := svc.Status()
		if err != nil {
			return err
		}
		fmt.Println(map[service.Status]string{service.StatusRunning: "running", service.StatusStopped: "stopped"}[s])
		return nil
	case "uninstall":
		_ = svc.Stop()
		return svc.Uninstall()
	default:
		return service.Control(svc, cmd)
	}
}
