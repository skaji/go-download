package main

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	yaml "github.com/goccy/go-yaml"
	archiver "github.com/mholt/archiver/v3"
)

type PackageDownloadURL struct {
	Mac   string `yaml:"mac"`
	Linux string `yaml:"linux"`
}

type PackageVersion struct {
	Command []string `yaml:"command"`
	Format  string   `yaml:"format"`
	Fixed   string   `yaml:"fixed"`

	formatRegexp *regexp.Regexp
	latest       string
	current      string
}

type Package struct {
	Name        string             `yaml:"name"`
	URL         string             `yaml:"url"`
	DownloadURL PackageDownloadURL `yaml:"download_url"`
	Version     PackageVersion     `yaml:"version"`

	downloadFile       string
	downloadBinaryFile string
	locateBinaryFile   string
}

func (p *Package) downloadURL(u string, v string) string {
	n := strings.TrimPrefix(v, "v")
	u = strings.ReplaceAll(u, "%v", "v"+n)
	u = strings.ReplaceAll(u, "%n", n)
	return u
}

func (p *Package) DownloadURLFor(myos string) string {
	v := p.Version.latest
	if p.Version.Fixed != "" {
		v = p.Version.Fixed
	}
	if myos == "linux" {
		return p.downloadURL(p.DownloadURL.Linux, v)
	}
	return p.downloadURL(p.DownloadURL.Mac, v)
}

func (p *Package) AlreadyLatestVersion() bool {
	current := p.Version.current
	latest := p.Version.latest
	if p.Version.Fixed != "" {
		latest = p.Version.Fixed
	}
	return strings.TrimPrefix(current, "v") == strings.TrimPrefix(latest, "v")
}

func (p *Package) Build() error {
	f := p.Version.Format
	reg, err := regexp.Compile(f)
	if err != nil {
		return err
	}
	p.Version.formatRegexp = reg
	return nil
}

func loadYAML(file string) ([]*Package, error) {
	c, err := ioutil.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var y struct {
		Packages []*Package `yaml:"packages"`
	}
	if err := yaml.Unmarshal(c, &y); err != nil {
		return nil, err
	}
	packages := y.Packages
	for _, p := range packages {
		if err := p.Build(); err != nil {
			return nil, err
		}
	}
	return packages, nil
}

type App struct {
	client           *http.Client
	noRedirectClient *http.Client
	workDir          string
	os               string
	binDir           string
}

func NewApp() (*App, error) {
	myos := ""
	switch runtime.GOOS {
	case "linux":
		myos = "linux"
	case "darwin":
		myos = "darwin"
	default:
		return nil, fmt.Errorf("unsupport")
	}
	dir, err := ioutil.TempDir("", "download")
	if err != nil {
		return nil, err
	}
	home := os.Getenv("HOME")
	if home == "" {
		return nil, fmt.Errorf("HOME is not set")
	}
	binDir := filepath.Join(home, "bin")
	if _, err := os.Stat(binDir); err != nil {
		if err := os.Mkdir(binDir, 0777); err != nil {
			return nil, err
		}
	}
	return &App{
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		noRedirectClient: &http.Client{
			Timeout: 5 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		workDir: dir,
		binDir:  binDir,
		os:      myos,
	}, nil
}

func (a *App) Cleanup() {
	defer os.RemoveAll(a.workDir)
}

func (a *App) LatestVersion(p *Package) (string, error) {
	u := fmt.Sprintf("%s/releases/latest", p.URL)
	res, err := a.noRedirectClient.Get(u)
	if err != nil {
		return "", err
	}
	io.Copy(ioutil.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode/100 != 3 {
		return "", fmt.Errorf("expect 3XX response, but %s, %s", res.Status, u)
	}
	if parts := strings.Split(res.Header.Get("Location"), "/"); len(parts) > 0 {
		return parts[len(parts)-1], nil
	}
	return "", fmt.Errorf("response does not contain Location Header")
}

func (a *App) CurrentVersion(p *Package) (string, error) {
	command := p.Version.Command
	if _, err := exec.LookPath(command[0]); err != nil {
		return "", err
	}
	out, err := exec.Command(command[0], command[1:]...).Output()
	ress := p.Version.formatRegexp.FindAllStringSubmatch(string(out), -1)
	if len(ress) == 0 {
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("cannot determine current version, check version format")
	}
	res := ress[0]
	return res[1], nil
}

func (a *App) Download(p *Package) (string, error) {
	u := p.DownloadURLFor(a.os)
	downloadFile := filepath.Join(a.workDir, p.Name, filepath.Base(u))
	if err := os.MkdirAll(filepath.Dir(downloadFile), 0777); err != nil {
		return "", err
	}
	file, err := os.Create(downloadFile)
	if err != nil {
		return "", err
	}
	err = func() error {
		res, err := a.client.Get(u)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if _, err := io.Copy(file, res.Body); err != nil {
			return err
		}
		if res.StatusCode/100 != 2 {
			return errors.New(res.Status)
		}
		return nil
	}()
	file.Close()
	if err != nil {
		os.Remove(downloadFile)
		return "", err
	}
	return downloadFile, nil
}

func (a *App) BinaryFile(p *Package) (string, error) {
	f := p.downloadFile
	if !(strings.HasSuffix(f, ".tar.gz") || strings.HasSuffix(f, ".tgz") || strings.HasSuffix(f, ".zip")) {
		return f, nil
	}

	extractDir := filepath.Join(filepath.Dir(f), "__extract")
	if err := os.Mkdir(extractDir, 0777); err != nil {
		return "", err
	}
	if err := archiver.Unarchive(f, extractDir); err != nil {
		return "", err
	}

	maxSize := int64(0)
	binaryFile := ""
	err := filepath.Walk(extractDir, func(path string, info os.FileInfo, err error) error {
		if size := info.Size(); size > maxSize {
			binaryFile = path
			maxSize = size
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return binaryFile, nil
}

func (a *App) LocateBinaryFile(p *Package) (string, error) {
	source := p.downloadBinaryFile
	if err := os.Chmod(source, 0755); err != nil {
		return "", err
	}
	target := filepath.Join(a.binDir, p.Name)
	if err := os.Rename(source, target); err != nil {
		return "", err
	}
	return target, nil
}

func (a *App) Log(p *Package, format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, p.Name+": "+format+"\n", args...)
}

func (a *App) Run(p *Package) error {
	var err error
	p.Version.current, err = a.CurrentVersion(p)
	if err == nil {
		a.Log(p, "current version is %s", p.Version.current)
	} else if errors.Is(err, exec.ErrNotFound) {
		a.Log(p, "not installed")
	} else {
		return err
	}
	if p.Version.latest, err = a.LatestVersion(p); err != nil {
		return err
	}
	a.Log(p, "latest version is %s", p.Version.latest)
	if p.AlreadyLatestVersion() {
		a.Log(p, "already have the latest version")
		return nil
	}

	a.Log(p, "\033[1;32mdownloading %s\033[m", p.DownloadURLFor(a.os))
	if p.downloadFile, err = a.Download(p); err != nil {
		return err
	}
	if p.downloadBinaryFile, err = a.BinaryFile(p); err != nil {
		return err
	}
	if p.locateBinaryFile, err = a.LocateBinaryFile(p); err != nil {
		return err
	}
	a.Log(p, "\033[1;32minstalled %s %s\033[m", p.locateBinaryFile, p.Version.latest)
	return nil
}

func run(file string) error {
	a, err := NewApp()
	if err != nil {
		return err
	}
	defer a.Cleanup()

	packages, err := loadYAML(file)
	if err != nil {
		return err
	}

	failChan := make(chan string)
	var fails []string
	go func() {
		for fail := range failChan {
			fails = append(fails, fail)
		}
	}()

	limit := make(chan struct{}, 3)
	for i := 0; i < 3; i++ {
		limit <- struct{}{}
	}
	var wg sync.WaitGroup

	for _, p := range packages {
		<-limit
		wg.Add(1)
		go func(p *Package) {
			defer func() {
				wg.Done()
				limit <- struct{}{}
			}()
			if err := a.Run(p); err != nil {
				a.Log(p, "failed, %s", err.Error())
				failChan <- p.Name
			}
		}(p)
	}
	wg.Wait()
	close(failChan)
	if len(fails) == 0 {
		return nil
	}
	return fmt.Errorf("failed to install %s", strings.Join(fails, ", "))
}

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" {
		fmt.Println("Usage: download packages.yml")
		os.Exit(1)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
