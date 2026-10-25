// pastecheck reads a paste or page through a WARP tunnel and prints which
// hosts its links point to and what kind they are (not the links themselves).
// Run: go run ./cmd/pastecheck <address>   (-v also prints the links)
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"sharkbite/internal/paste"
	"sharkbite/internal/tunnel"
	"sharkbite/internal/warp"
)

func main() {
	verbose := flag.Bool("v", false, "print every link")
	dump := flag.String("dump", "", "save the first page link's HTML to this file (for writing a host handler)")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Println("usage: pastecheck [-v] <address>")
		os.Exit(2)
	}
	dir, _ := os.UserConfigDir()
	acc, err := warp.LoadOrRegister(filepath.Join(dir, "SharkBiteTorrent", "warp.json"))
	if err != nil {
		fmt.Println("register:", err)
		os.Exit(1)
	}
	t, err := tunnel.Start(tunnel.Config{
		PrivateKey:    acc.PrivateKey,
		PeerPublicKey: acc.PeerPublicKey,
		Endpoint:      acc.Endpoint,
		Addresses:     []string{acc.AddressV4, acc.AddressV6},
		DNS:           []string{"1.1.1.1", "1.0.0.1"},
	})
	if err != nil {
		fmt.Println("tunnel:", err)
		os.Exit(1)
	}
	defer t.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	links, err := paste.Find(ctx, t.HTTPClient(), flag.Arg(0))
	if err != nil {
		fmt.Printf("read: %q\n", err.Error())
		os.Exit(1)
	}
	fmt.Printf("%d links\n", len(links))
	count := map[string]int{}
	for _, l := range links {
		count[fmt.Sprintf("%-8s %q", l.Kind, l.Host)]++
		if *verbose {
			fmt.Printf("  %s %q\n", l.Kind, l.URL) // quoted: a page's links could carry terminal control characters
		}
	}
	keys := make([]string, 0, len(count))
	for k := range count {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%4d  %s\n", count[k], k)
	}
	if *dump == "" {
		return
	}
	for _, l := range links {
		if l.Kind == paste.Page {
			page, err := paste.Read(ctx, t.HTTPClient(), l.URL)
			if err != nil {
				fmt.Printf("dump: %q\n", err.Error())
				os.Exit(1)
			}
			if err := os.WriteFile(*dump, []byte(page), 0o600); err != nil {
				fmt.Printf("dump: %q\n", err.Error())
				os.Exit(1)
			}
			fmt.Printf("saved %s (%d bytes)\n", *dump, len(page))
			return
		}
	}
}
