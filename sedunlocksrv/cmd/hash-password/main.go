package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

func main() {
	pwArg := flag.String("password", "", "expert password to hash")
	flag.Parse()

	password := strings.TrimSpace(*pwArg)
	if password == "" {
		password = strings.TrimSpace(os.Getenv("EXPERT_PASSWORD_INPUT"))
	}
	if password == "" {
		fmt.Fprintln(os.Stderr, "missing password input")
		os.Exit(1)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to hash password: %v\n", err)
		os.Exit(1)
	}

	fmt.Println(string(hash))
}
