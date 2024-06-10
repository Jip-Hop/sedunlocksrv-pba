package main

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"time"
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

func fileExists(filename string) bool {
    _, err := os.Stat(filename)
    return err == nil
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

func cmdExecStdIO(args ...string) {

	baseCmd := args[0]
	cmdArgs := args[1:]

	cmd := exec.Command(baseCmd, cmdArgs...)

	// create a pipe for the output of the script
	cmdReader, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error creating StdoutPipe for Cmd", err)
		return
	}

	err = cmd.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error starting Cmd", err)
		return
	}

	scanner := bufio.NewScanner(cmdReader)
	for scanner.Scan() {
		m := scanner.Text()
		fmt.Println(m)
	}

	cmd.Wait()
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
			if fileExists("/home/tc/partid-efi") {
				cmdExec(w, "./efiupdate.sh")
			}
			cmdExec(w, "./reboot.sh")
		} else {
			psw := r.FormValue("psw")
			if r.FormValue("action") == "unlock" {
				cmdExec(w, "./opal-functions.sh", psw)
			} else if r.FormValue("action") == "change-pwd" {
				newpsw := r.FormValue("newpsw")
				newpsw2 := r.FormValue("newpsw2")
				cmdExec(w, "./opal-functions.sh", psw, newpsw, newpsw2)
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

func passwordInput() {
	var b []byte = make([]byte, 1)
	var password_buffer bytes.Buffer
	for {
		fmt.Printf("\nKey in SED password and press Enter anytime to unlock\n")
		fmt.Printf("Note that keystrokes won't be echoed on the screen\n")
		fmt.Printf("Press ESC anytime to reboot\n")
		fmt.Printf("Press Ctrl-D anytime to shutdown\n")
	out:
		for {
			os.Stdin.Read(b)
			switch bi := b[0]; bi {
			case 4: // CTRL-D key
				fmt.Println("Shutting down in 3 seconds")
				time.Sleep(3 * time.Second)
				cmdExecStdIO("./shutdown.sh")
			case 27: // ESC key
				if fileExists("/home/tc/partid-efi") {
					fmt.Println("Reinstall EFI")
					cmdExecStdIO("./efiupdate.sh")
				}
				fmt.Println("Rebooting in 3 seconds")
				time.Sleep(3 * time.Second)
				cmdExecStdIO("./reboot.sh")
			case 10: // ENTER key
				fmt.Println("Password entered. Trying to unlock disk with password...")
				cmdExecStdIO("./opal-functions.sh", password_buffer.String())
				password_buffer.Reset()
				break out
			case 127: // BACKSPACE key
				if password_buffer.Len() > 0 {
					password_buffer.Truncate(password_buffer.Len() - 1)
				}
			default:
				password_buffer.Write(b)
			}
		}
	}
}

func httpServer() {
	ip := getOutboundIP()
	// redirect every http request to https
	go http.ListenAndServe(httpAddr, http.HandlerFunc(redirect))

	fmt.Printf("\n%sStarting SED unlock server at %s%s%s%s...%s\n", green, magenta, ip, httpsAddr, green, normal)

	l, err := net.Listen("tcp", httpsAddr)
	if err != nil {
		log.Fatal(err)
	}

	// serve index (and anything else) as https
	mux := http.NewServeMux()
	mux.HandleFunc("/", index)

	fmt.Printf("\nReady to connect\n")

	log.Fatal(http.ServeTLS(l, mux, "server.crt", "server.key"))
}

func sshServer() {
	if _, err := os.Stat("/usr/local/sbin/dropbear"); err == nil {
		// Start SSH server listening on port 2222
		fmt.Printf("\n%sStarting SSH SED unlock service on port %s2222%s\n", green, magenta, normal)
		cmd := exec.Command("/usr/local/sbin/dropbear", "-s", "-g", "-w", "-j", "-k", "-p", "2222", "-b", "/usr/local/etc/dropbear/banner")
		cmd.Run()
	}
}

func waitForNetworkAndStartNetServices() {
	fmt.Printf("\nWait for network connection...\n")
	cmd := exec.Command("./wait-for-network.sh")
	cmd.Run()
	go httpServer()
	go sshServer()
}

func main() {

	go waitForNetworkAndStartNetServices()
	time.Sleep(2 * time.Second)
	passwordInput()

}
