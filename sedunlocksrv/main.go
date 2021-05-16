package main

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
)

var httpAddr string = ":80"
var httpsAddr string = ":443"

// ANSI colors
var green string = "\033[1;32m"
var magenta string = "\033[1;35m"
var normal string = "\033[0;39m"

func redirect(w http.ResponseWriter, req *http.Request) {
	// remove/add not default ports from req.Host
	target := "https://" + req.Host + req.URL.Path
	if len(req.URL.RawQuery) > 0 {
		target += "?" + req.URL.RawQuery
	}
	log.Printf("redirect to: %s", target)
	http.Redirect(w, req, target, http.StatusPermanentRedirect)
}

func cmdExec(w http.ResponseWriter, args ...string) {

	baseCmd := args[0]
	cmdArgs := args[1:]

	cmd := exec.Command(baseCmd, cmdArgs...)

	// create a pipe for the output of the script
	cmdReader, err := cmd.StdoutPipe()
	// also capture Stderr output
	cmd.Stderr = cmd.Stdout
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error creating StdoutPipe for Cmd", err)
		return
	}

	scanner := bufio.NewScanner(cmdReader)
	go func() {
		for scanner.Scan() {
			fmt.Fprintf(w, "%s\n", scanner.Text())

			if f, ok := w.(http.Flusher); ok {
				// send data immediately
				f.Flush()
			}
		}
	}()

	err = cmd.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error starting Cmd", err)
		return
	}

	err = cmd.Wait()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error waiting for Cmd", err)
		return
	}
}

func index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.Error(w, "404 not found.", http.StatusNotFound)
		return
	}

	switch r.Method {
	case "GET":
		http.ServeFile(w, r, "index.html")
	case "POST":
		// Call ParseForm() to parse the raw query and update r.PostForm and r.Form.
		if err := r.ParseForm(); err != nil {
			fmt.Fprintf(w, "ParseForm() err: %v", err)
			return
		}
		if r.FormValue("action") == "reboot" {
			cmdExec(w, "./reboot.sh")
		} else {
			psw := r.FormValue("psw")
			if r.FormValue("action") == "unlock" {
				cmdExec(w, "./unlock.sh", psw)
			} else if r.FormValue("action") == "change-pwd" {
				newpsw := r.FormValue("newpsw")
				newpsw2 := r.FormValue("newpsw2")
				cmdExec(w, "./unlock.sh", psw, newpsw, newpsw2)
			}
		}

	default:
		fmt.Fprintf(w, "Sorry, only GET and POST methods are supported.")
	}
}

// Get preferred outbound ip of this machine
// Connection to 192.168.1.1 is not actually made
func getOutboundIP() net.IP {
	conn, err := net.Dial("udp", "192.168.1.1:80")
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)

	return localAddr.IP
}

func main() {

	// redirect every http request to https
	go http.ListenAndServe(httpAddr, http.HandlerFunc(redirect))

	fmt.Printf("\n%sStarting SED unlock server at %s%s%s%s...%s\n", green, magenta, getOutboundIP(), httpsAddr, green, normal)

	l, err := net.Listen("tcp", httpsAddr)
	if err != nil {
		log.Fatal(err)
	}

	// serve index (and anything else) as https
	mux := http.NewServeMux()
	mux.HandleFunc("/", index)

	fmt.Printf("\nReady to connect\n")

	log.Fatal(http.ServeTLS(l, mux, "server.crt", "server.key"))

	// ServeTLS blocks, so this point is never reached
}
