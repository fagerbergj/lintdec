package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var releaseAPI = "https://api.github.com/repos/fagerbergj/lintdec/releases/latest"

type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func updateCmd() int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	if err := selfUpdate(exe, releaseAPI, runtime.GOOS, runtime.GOARCH); err != nil {
		fmt.Fprintln(os.Stderr, "update failed:", err)
		return 2
	}
	return 0
}

func selfUpdate(exePath, apiURL, goos, goarch string) error {
	c := &http.Client{Timeout: 2 * time.Minute}
	rel, err := fetchLatestRelease(c, apiURL)
	if err != nil {
		return err
	}
	latest := strings.TrimPrefix(rel.TagName, "v")
	cur := strings.TrimPrefix(version, "v")
	if cur != "dev" && cur == latest {
		fmt.Printf("lintdec %s is already up to date\n", cur)
		return nil
	}
	assetName := fmt.Sprintf("lintdec_%s_%s", goos, goarch)
	binURL, sumURL := findReleaseAssets(rel, assetName)
	if binURL == "" {
		return fmt.Errorf("release %s has no asset %s", rel.TagName, assetName)
	}
	if sumURL == "" {
		return fmt.Errorf("release %s has no checksums.txt", rel.TagName)
	}
	wantSum, err := fetchExpectedSum(c, sumURL, assetName)
	if err != nil {
		return err
	}
	if err := downloadAndReplace(c, binURL, exePath, wantSum); err != nil {
		return err
	}
	fmt.Printf("updated lintdec %s → %s\n", cur, latest)
	return nil
}

func fetchLatestRelease(c *http.Client, apiURL string) (ghRelease, error) {
	var rel ghRelease
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return rel, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	res, err := c.Do(req)
	if err != nil {
		return rel, err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 400))
		return rel, fmt.Errorf("HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	if err := json.NewDecoder(res.Body).Decode(&rel); err != nil {
		return rel, err
	}
	return rel, nil
}

func findReleaseAssets(rel ghRelease, assetName string) (binURL, sumURL string) {
	for _, a := range rel.Assets {
		switch a.Name {
		case assetName:
			binURL = a.URL
		case "checksums.txt":
			sumURL = a.URL
		}
	}
	return binURL, sumURL
}

func fetchExpectedSum(c *http.Client, sumURL, assetName string) (string, error) {
	res, err := c.Get(sumURL)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		return "", fmt.Errorf("checksums.txt HTTP %d", res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == assetName {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("no checksum for %s in checksums.txt", assetName)
}

func downloadAndReplace(c *http.Client, binURL, exePath, wantSum string) error {
	res, err := c.Get(binURL)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		return fmt.Errorf("download HTTP %d", res.StatusCode)
	}
	tmp, err := os.CreateTemp(filepath.Dir(exePath), ".lintdec-update-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), res.Body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSum {
		return fmt.Errorf("checksum mismatch: got %s, want %s", got, wantSum)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	return os.Rename(tmpName, exePath)
}
