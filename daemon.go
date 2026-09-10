package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const daemonLabel = "local.wa-mirror"

func plistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", daemonLabel+".plist")
}

func plistBody(binary, logPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>serve</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>30</integer>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, daemonLabel, binary, logPath, logPath)
}

// installDaemon writes the launch agent and starts it, so the mirror survives
// reboots and keeps receiving while no terminal is open.
func installDaemon() error {
	binary, err := exec.LookPath("wa")
	if err != nil {
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("cannot locate the wa binary: %w", err)
		}
		binary = self
	}
	home, _ := os.UserHomeDir()
	logPath := filepath.Join(home, ".wa", "wa.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return err
	}

	path := plistPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(plistBody(binary, logPath)), 0o644); err != nil {
		return err
	}

	uid := fmt.Sprintf("gui/%d", os.Getuid())
	_ = exec.Command("launchctl", "bootout", uid+"/"+daemonLabel).Run()
	if out, err := exec.Command("launchctl", "bootstrap", uid, path).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, out)
	}
	fmt.Printf("Daemon installed and started.\n  agent: %s\n  log:   %s\n", path, logPath)
	return nil
}

func uninstallDaemon() error {
	uid := fmt.Sprintf("gui/%d", os.Getuid())
	_ = exec.Command("launchctl", "bootout", uid+"/"+daemonLabel).Run()
	if err := os.Remove(plistPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Println("Daemon stopped and removed.")
	return nil
}

// pauseDaemon stops the launch agent if it is loaded and returns the function
// that brings it back, so pairing never competes with the background mirror.
func pauseDaemon() func() {
	path := plistPath()
	if _, err := os.Stat(path); err != nil {
		return func() {}
	}
	uid := fmt.Sprintf("gui/%d", os.Getuid())
	if err := exec.Command("launchctl", "bootout", uid+"/"+daemonLabel).Run(); err != nil {
		return func() {}
	}
	return func() {
		if out, err := exec.Command("launchctl", "bootstrap", uid, path).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "wa: could not restart the daemon: %v: %s\n", err, out)
			return
		}
		fmt.Println("Background mirror restarted.")
	}
}
