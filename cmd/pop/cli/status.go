package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/myelnet/pop/node"
	"github.com/peterbourgon/ff/v2/ffcli"
)

var statusCmd = &ffcli.Command{
	Name:      "status",
	ShortHelp: "Print the state of any ongoing transaction",
	LongHelp: strings.TrimSpace(`

The 'pop status' command prints all the files that have been added to a transaction DAG. Files that have
been chunked and staged in the blockstore but not yet committed to be pushed to the network.

`),
	Exec: runStatus,
}

func runStatus(ctx context.Context, args []string) error {
	c, cc, ctx, cancel := connect(ctx)
	defer cancel()

	src := make(chan *node.StatusResult, 1)
	cc.SetNotifyCallback(func(n node.Notify) {
		if sr := n.StatusResult; sr != nil {
			src <- sr
		}
	})
	go receive(ctx, cc, c)

	cc.Status(&node.StatusArgs{})
	select {
	case sr := <-src:
		if sr.Err != "" {
			return errors.New(sr.Err)
		}
		if sr.Entries == "" {
			fmt.Printf("Nothing to pack, workdag clean.\n")
			return nil
		}
		fmt.Printf("Staged for storage:\n")
		// Output is already formatted but should move it here
		fmt.Printf("%s\n", sr.Entries)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
