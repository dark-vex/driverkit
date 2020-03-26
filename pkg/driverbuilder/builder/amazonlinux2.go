package builder

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"
	"text/template"

	"database/sql"

	"github.com/falcosecurity/driverkit/pkg/kernelrelease"
	_ "github.com/mattn/go-sqlite3" // Why do you want me to justify? Leave me alone :)
	logger "github.com/sirupsen/logrus"
)

type amazonlinux2 struct {
}

type amazonlinux struct {
}

// TargetTypeAmazonLinux2 identifies the AmazonLinux2 target.
const TargetTypeAmazonLinux2 Type = "amazonlinux2"

// TargetTypeAmazonLinux identifies the AmazonLinux target.
const TargetTypeAmazonLinux Type = "amazonlinux"

func init() {
	BuilderByTarget[TargetTypeAmazonLinux2] = &amazonlinux2{}
	BuilderByTarget[TargetTypeAmazonLinux] = &amazonlinux{}
}

const amazonlinuxTemplate = `
{{ range $url := .KernelDownloadURLs }}
echo {{ $url }}
{{ end }}
`

type amazonlinuxTemplateData struct {
	DriverBuildDir     string
	ModuleDownloadURL  string
	KernelDownloadURLs []string
	BuildModule        bool
	BuildProbe         bool
}

// Script compiles the script to build the kernel module and/or the eBPF probe.
func (a amazonlinux2) Script(c Config) (string, error) {
	return script(c, TargetTypeAmazonLinux2)
}

// Script compiles the script to build the kernel module and/or the eBPF probe.
func (a amazonlinux) Script(c Config) (string, error) {
	return script(c, TargetTypeAmazonLinux)
}

func script(c Config, targetType Type) (string, error) {
	t := template.New(string(targetType))
	parsed, err := t.Parse(amazonlinuxTemplate)
	if err != nil {
		return "", err
	}

	kv := kernelrelease.FromString(c.Build.KernelRelease)

	// Check (and filter) existing kernels before continuing
	packages, err := fetchAmazonLinuxPackagesURLs(kv, c.Build.Architecture, targetType)
	if err != nil {
		return "", err
	}
	if len(packages) != 2 {
		return "", fmt.Errorf("target %s needs to find both kernel and kernel-devel packages", targetType)
	}
	fmt.Println(packages)
	urls, err := getResolvingURLs(packages)
	if err != nil {
		return "", err
	}

	td := amazonlinuxTemplateData{
		DriverBuildDir:     DriverDirectory,
		ModuleDownloadURL:  moduleDownloadURL(c),
		KernelDownloadURLs: urls,
		BuildModule:        len(c.Build.ModuleFilePath) > 0,
		BuildProbe:         len(c.Build.ProbeFilePath) > 0,
	}

	buf := bytes.NewBuffer(nil)
	err = parsed.Execute(buf, td)
	if err != nil {
		return "", err
	}
	return buf.String(), nil
}

var reposByTarget = map[Type][]string{
	TargetTypeAmazonLinux2: []string{
		"2.0",
		"latest",
	},
	TargetTypeAmazonLinux: []string{
		"latest/updates",
		"latest/main",
		"2017.03/updates",
		"2017.03/main",
		"2017.09/updates",
		"2017.09/main",
		"2018.03/updates",
		"2018.03/main",
	},
}

var baseByTarget = map[Type]string{
	TargetTypeAmazonLinux:  "http://repo.us-east-1.amazonaws.com/%s",
	TargetTypeAmazonLinux2: "http://amazonlinux.us-east-1.amazonaws.com/2/core/%s/%s",
}

func fetchAmazonLinuxPackagesURLs(kv kernelrelease.KernelRelease, arch string, targetType Type) ([]string, error) {
	urls := []string{}
	visited := map[string]bool{}

	for _, v := range reposByTarget[targetType] {
		var baseURL string
		switch targetType {
		case TargetTypeAmazonLinux:
			baseURL = fmt.Sprintf("http://repo.us-east-1.amazonaws.com/%s", v)
		case TargetTypeAmazonLinux2:
			baseURL = fmt.Sprintf("http://amazonlinux.us-east-1.amazonaws.com/2/core/%s/%s", v, arch)
		default:
			return nil, fmt.Errorf("unsupported target")
		}

		mirror := fmt.Sprintf("%s/%s", baseURL, "mirror.list")
		logger.WithField("url", mirror).WithField("version", v).Debug("looking for repo...")
		// Obtain the repo URL by getting mirror URL content
		mirrorRes, err := http.Get(mirror)
		if err != nil {
			return nil, err
		}
		defer mirrorRes.Body.Close()

		var repo string
		scanner := bufio.NewScanner(mirrorRes.Body)
		if scanner.Scan() {
			repo = scanner.Text()
		}
		if repo == "" {
			return nil, fmt.Errorf("repository not found")
		}

		ext := "gz"
		if targetType == TargetTypeAmazonLinux {
			ext = "bz2"
		}
		repoDatabaseURL := fmt.Sprintf("%s/repodata/primary.sqlite.%s", strings.TrimSuffix(string(repo), "\n"), ext)
		repoDatabaseURL = strings.ReplaceAll(repoDatabaseURL, "$basearch", arch)

		if _, ok := visited[repoDatabaseURL]; ok {
			continue
		}
		// Download the repo database
		repoRes, err := http.Get(repoDatabaseURL)
		logger.WithField("url", repoDatabaseURL).Debug("downloading ...")
		if err != nil {
			return nil, err
		}
		defer repoRes.Body.Close()
		visited[repoDatabaseURL] = true
		// Decompress the database
		var unzipFunc func(io.Reader) ([]byte, error)
		if targetType == TargetTypeAmazonLinux {
			unzipFunc = bunzip
		} else {
			unzipFunc = gunzip
		}
		dbBytes, err := unzipFunc(repoRes.Body)
		if err != nil {
			return nil, err
		}
		// Create the temporary database file
		dbFile, err := ioutil.TempFile(os.TempDir(), fmt.Sprintf("%s-*.sqlite", targetType))
		if err != nil {
			return nil, err
		}
		defer os.Remove(dbFile.Name())
		if _, err := dbFile.Write(dbBytes); err != nil {
			return nil, err
		}
		// Open the database
		db, err := sql.Open("sqlite3", dbFile.Name())
		if err != nil {
			return nil, err
		}
		defer db.Close()
		logger.WithField("db", dbFile.Name()).Debug("connecting to database...")
		// Query the database
		// fixme > it seems they should always be 2 URLs (and the most recent ones?)
		// https://github.com/draios/sysdig/blob/fb08e7f59cca570383bdafb5de96824b8a2e9e6b/probe-builder/kernel-crawler.py#L414
		rel := strings.TrimPrefix(strings.TrimSuffix(kv.FullExtraversion, fmt.Sprintf(".%s", arch)), "-")
		q := fmt.Sprintf("SELECT location_href FROM packages WHERE name LIKE 'kernel%%' AND name NOT LIKE 'kernel-livepatch%%' AND name NOT LIKE '%%doc%%' AND name NOT LIKE '%%tools%%' AND name NOT LIKE '%%headers%%' AND version='%s' AND release='%s'", kv.Fullversion, rel)
		stmt, err := db.Prepare(q)
		if err != nil {
			return nil, err
		}
		defer stmt.Close()
		rows, err := stmt.Query()
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var href string
			err = rows.Scan(&href)
			if err != nil {
				log.Fatal(err)
			}
			urls = append(urls, fmt.Sprintf("%s/%s", baseURL, href))
		}

		if err := dbFile.Close(); err != nil {
			return nil, err
		}
	}

	return urls, nil
}

func gunzip(data io.Reader) (res []byte, err error) {
	var r io.Reader
	r, err = gzip.NewReader(data)
	if err != nil {
		return
	}

	var b bytes.Buffer
	_, err = b.ReadFrom(r)
	if err != nil {
		return
	}

	res = b.Bytes()

	return
}

func bunzip(data io.Reader) (res []byte, err error) {
	var r io.Reader
	r = bzip2.NewReader(data)

	var b bytes.Buffer
	_, err = b.ReadFrom(r)
	if err != nil {
		return
	}

	res = b.Bytes()

	return
}