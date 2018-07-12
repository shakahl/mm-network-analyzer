// network-analyzer collects data about the machine it is running on and its
// network connection to help diagnose routing, DNS, and other issues to
// MaxMind servers.
package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sync"

	"github.com/pkg/errors"
)

const (
	host        = "geoip.maxmind.com"
	zipFileName = "mm-network-analysis.zip"
)

type zipFile struct {
	name     string
	contents []byte
}

type analyzer struct {
	zipWriter *zip.Writer
	zipFile   *os.File

	// We use mutexes as it is a bit easier to handle writing
	// in the main go routine
	errorsMutex sync.Mutex
	errors      []error

	zipFilesMutex sync.Mutex
	zipFiles      []*zipFile
}

func main() {
	a, err := newAnalyzer()
	if err != nil {
		log.Println(err)
	}

	tasks := []func(){
		// Ideally, we would just be doing these using Go's httptrace so that
		// they don't require curl, but this is good enough for now.
		a.createStoreCommand(host+"-curl-ipv4.txt", "curl", "-4", "--trace-time", "--trace-ascii", "-", "--user-agent", os.Args[0], host),
		a.createStoreCommand(host+"-curl-ipv6.txt", "curl", "-6", "--trace-time", "--trace-ascii", "-", "--user-agent", os.Args[0], host),

		a.createStoreCommand(host+"-dig.txt", "dig", "-4", "+all", host, "A", host, "AAAA"),
		a.createStoreCommand(host+"-dig-google.txt", "dig", "-4", "+all", "@8.8.8.8", host, "A", host, "AAAA"),
		a.createStoreCommand(host+"-dig-google-trace.txt", "dig", "-4", "+all", "+trace", "@8.8.8.8", host, "A", host, "AAAA"),

		// CF support want this, but there are multiple boxes in the pool
		// so no guarantee we will see the same results as a customer
		// or hit a broken NS, if there is one
		a.createStoreCommand(host+"-dig-cloudflare-josh.txt", "dig", "-4", host, "@josh.ns.cloudflare.com", "+nsid"),
		a.createStoreCommand(host+"-dig-cloudflare-kim.txt", "dig", "-4", host, "@kim.ns.cloudflare.com", "+nsid"),

		// rfc4892 - gives geographic region
		a.createStoreCommand(host+"-dig-cloudflare-josh-rfc4892.txt", "dig", "-4", "CH", "TXT", "id.server", host, "@josh.ns.cloudflare.com", "+nsid"),
		a.createStoreCommand(host+"-dig-cloudflare-kim-rfc4892.txt", "dig", "-4", "CH", "TXT", "id.server", host, "@kim.ns.cloudflare.com", "+nsid"),

		// CF support want this, too. Don't see what it's useful for
		// unless we have customers using this service
		// and they happen to hit the same box in the pool
		a.createStoreCommand(host+"-dig-cloudflare.txt", "dig", "-4", "@1.1.1.1", "CH", "TXT", "hostname.cloudflare", "+short"),

		a.createStoreCommand("ip-addr.txt", "ip", "addr"),
		a.createStoreCommand("ip-route.txt", "ip", "route"),
		a.createStoreCommand(host+"-mtr-ipv4.json", "mtr", "-j", "-4", host),
		a.createStoreCommand(host+"-mtr-ipv6.json", "mtr", "-j", "-6", host),
		a.createStoreCommand(host+"-ping-ipv4.txt", "ping", "-4", "-c", "30", host),
		a.createStoreCommand(host+"-ping-ipv6.txt", "ping", "-6", "-c", "30", host),
		a.createStoreCommand(host+"-tracepath.txt", "tracepath", host),
		a.addIP,
		a.addResolvConf,
	}

	var wg sync.WaitGroup
	for _, task := range tasks {
		wg.Add(1)
		go func(task func()) {
			task()
			wg.Done()
		}(task)
	}

	wg.Wait()

	err = a.addErrors()
	if err != nil {
		log.Println(err)
	}

	err = a.writeFiles()
	if err != nil {
		log.Println(err)
	}

	err = a.close()
	if err != nil {
		log.Println(err)
	}
}

func newAnalyzer() (*analyzer, error) {
	f, err := os.OpenFile(zipFileName, os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		return nil, errors.Wrap(err, "error opening "+zipFileName)
	}

	return &analyzer{
		zipWriter: zip.NewWriter(f),
		zipFile:   f,
	}, nil
}

func (a *analyzer) close() error {
	err := a.zipWriter.Close()
	if err != nil {
		return errors.Wrap(err, "error closing zip file writer")
	}
	err = a.zipFile.Close()
	if err != nil {
		return errors.Wrap(err, "error closing zip file")
	}
	return nil
}

func (a *analyzer) storeFile(name string, contents []byte) {
	a.zipFilesMutex.Lock()
	a.zipFiles = append(a.zipFiles, &zipFile{name: name, contents: contents})
	a.zipFilesMutex.Unlock()
}

func (a *analyzer) storeError(err error) {
	a.errorsMutex.Lock()
	a.errors = append(a.errors, err)
	a.errorsMutex.Unlock()
}

func (a *analyzer) writeFile(zf *zipFile) error {
	f, err := a.zipWriter.Create(zf.name)
	if err != nil {
		return errors.Wrap(err, "error creating "+zf.name+" in zip file")
	}
	_, err = f.Write(zf.contents)
	if err != nil {
		return errors.Wrap(err, "error writing "+zf.name+" to zip file")
	}
	return nil
}

func (a *analyzer) createStoreCommand(
	f, command string,
	args ...string,
) func() {
	return func() {
		cmd := exec.Command(command, args...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			a.storeError(errors.Wrapf(err, "error getting data for %s", f))
		}
		a.storeFile(f, output)
	}
}

func (a *analyzer) addIP() {
	resp, err := http.Get("http://" + host + "/app/update_getipaddr")
	if err != nil {
		err = errors.Wrap(err, "error getting IP address")
		a.storeError(err)
		return
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		err = errors.Wrap(err, "error reading IP address body")
		a.storeError(err)
		return
	}

	a.storeFile("ip-address.txt", body)
}

func (a *analyzer) addResolvConf() {
	contents, err := ioutil.ReadFile("/etc/resolv.conf")
	if err != nil {
		err = errors.Wrap(err, "error reading resolv.conf")
		a.storeError(err)
		return
	}
	a.storeFile("resolv.conf", contents)
}

func (a *analyzer) addErrors() error {
	a.errorsMutex.Lock()
	defer a.errorsMutex.Unlock()
	if len(a.errors) == 0 {
		return nil
	}
	buf := new(bytes.Buffer)
	for _, storedErr := range a.errors {
		_, err := fmt.Fprintf(buf, "%+v\n\n----------\n\n", storedErr)
		if err != nil {
			return errors.Wrap(err, "error writing errors.txt buffer")
		}
	}
	a.storeFile("errors.txt", buf.Bytes())
	return nil
}

func (a *analyzer) writeFiles() error {
	a.errorsMutex.Lock()
	defer a.errorsMutex.Unlock()
	for _, zf := range a.zipFiles {
		err := a.writeFile(zf)
		if err != nil {
			return err
		}
	}
	return nil
}