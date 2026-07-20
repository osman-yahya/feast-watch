// Package mother wires the store, API, and background jobs; also hosts the
// `feast-watch generate` CLI used on the mother host without the panel.
package mother

import (
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/osman-yahya/feast-watch/mother/store"
)

// RunGenerate implements `feast-watch generate --name=X`: create-or-fetch the
// server and return the one-liner install command with the mother IP embedded.
func RunGenerate(st *store.Store, publicAddr string, args []string) (string, error) {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	name := fs.String("name", "", "server name")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if strings.TrimSpace(*name) == "" {
		return "", errors.New("--name is required")
	}

	srv, err := st.ServerByName(*name)
	if errors.Is(err, store.ErrNotFound) {
		srv, err = st.AddServer(*name)
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("curl -sSLk https://%s/install/%s.sh | sudo bash", publicAddr, srv.Token), nil
}
