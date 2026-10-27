package claude

import (
	"runtime"
	"testing"
)

func TestIsDesktopProcess(t *testing.T) {
	type tc struct {
		exe  string
		cmd  []string
		want bool
	}
	var cases []tc
	switch runtime.GOOS {
	case "windows":
		cases = []tc{
			{`C:\Program Files\WindowsApps\Claude_3.6556.2.0_x64__pzs8sxrjxfjjc\app\Claude.exe`, nil, true},
			{`C:\Users\J\AppData\Local\AnthropicClaude\app-1.0\claude.exe`, nil, true},
			{`C:\Users\J\AppData\Local\Claude-Data\claude_x\claude-code\2.1.308\abc\claude.exe`, nil, false}, // Claude Code CLI
			{`C:\Windows\notepad.exe`, []string{"notepad", `C:\Users\J\AppData\Roaming\Claude\x.json`}, false},
		}
	case "linux":
		cases = []tc{
			{"/usr/lib/claude-desktop/node_modules/electron/dist/electron", []string{"electron", "/usr/lib/claude-desktop/app.asar"}, true},
			{"/usr/bin/claude-desktop", []string{"claude-desktop"}, true},
			{"/tmp/.mount_claudeXYZ/claude", []string{"/tmp/.mount_claudeXYZ/claude"}, true},
			{"/home/j/.local/bin/claude", []string{"claude"}, false},                                               // Claude Code CLI
			{"/home/j/.config/Claude/claude-code/2.1/claude", []string{"claude"}, false},                           // CLI bundled by the app
			{"/usr/bin/vim", []string{"vim", "/home/j/.config/claude-desktop/x.json"}, false},                      // editor
			{"/usr/bin/tail", []string{"tail", "-f", "/home/j/.config/claude-desktop/logs/main.log"}, false},       // shell command
			{"/usr/bin/bash", []string{"bash", "-c", "cd ~/.config/claude-desktop && ls"}, false},                  // shell
			{"/opt/google/chrome/chrome", []string{"chrome", "https://github.com/x/claude-desktop-debian"}, false}, // browser tab
		}
	default:
		t.Skip("no cases for " + runtime.GOOS)
	}
	for _, c := range cases {
		if got := IsDesktopProcess(c.exe, c.cmd); got != c.want {
			t.Errorf("IsDesktopProcess(%q, %q) = %v, want %v", c.exe, c.cmd, got, c.want)
		}
	}
}

func TestStoreAppID(t *testing.T) {
	got := storeAppID(`C:\Program Files\WindowsApps\Claude_3.6556.2.0_x64__pzs8sxrjxfjjc\app\Claude.exe`)
	if got != "Claude_pzs8sxrjxfjjc!Claude" {
		t.Fatalf("storeAppID = %q", got)
	}
}
