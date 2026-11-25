// Command llmgate-keygen creates a client API key and the hash to put in config.
//
// The plaintext key is printed once and never stored by llmgate.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/anujsit1999/llmgate/auth"
)

func main() {
	id := flag.String("id", "my-key", "key id")
	tenant := flag.String("tenant", "default", "tenant the key belongs to")
	rpm := flag.Int("rpm", 60, "requests per minute")
	flag.Parse()

	key, hash, err := auth.Generate()
	if err != nil {
		fmt.Fprintln(os.Stderr, "keygen:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "API key (shown once; give it to the client, do not commit it):")
	fmt.Println(key)
	fmt.Fprintf(os.Stderr, "\nAdd this to the `keys:` section of your config:\n\n")
	fmt.Fprintf(os.Stderr, "  - id: %s\n    tenant: %s\n    hash: %s\n    rpm: %d\n", *id, *tenant, hash, *rpm)
}
