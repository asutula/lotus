package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/api/client"
	"github.com/filecoin-project/lotus/build"
	"github.com/filecoin-project/lotus/lib/jsonrpc"
	"github.com/filecoin-project/lotus/node/repo"
	cid "github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log"
	manet "github.com/multiformats/go-multiaddr-net"
	"golang.org/x/xerrors"
	"gopkg.in/urfave/cli.v2"
)

var log = logging.Logger("lotus-seed")

func main() {
	logging.SetLogLevel("*", "INFO")

	log.Info("Starting health agent")

	local := []*cli.Command{
		watchHeadCmd,
	}

	app := &cli.App{
		Name:     "lotus-health",
		Usage:    "Tools for monitoring lotus daemon health",
		Version:  build.UserVersion,
		Commands: local,
	}

	if err := app.Run(os.Args); err != nil {
		log.Warn(err)
		return
	}
}

var watchHeadCmd = &cli.Command{
	Name: "watch-head",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "repo",
			Value: "~/.lotus",
			Usage: "lotus repo path",
		},
		&cli.IntFlag{
			Name:  "threshold",
			Value: 3,
			Usage: "number of times head remains unchanged before failing health check",
		},
		&cli.IntFlag{
			Name:  "interval",
			Value: 45,
			Usage: "interval in seconds between chain head checks",
		},
		&cli.StringFlag{
			Name:  "systemd-unit",
			Value: "lotus-daemon.service",
			Usage: "systemd unit name to restart on health check failure",
		},
	},
	Action: func(c *cli.Context) error {
		repo := c.String("repo")
		threshold := c.Int("threshold")
		interval := time.Duration(c.Int("interval"))
		name := c.String("systemd-unit")

		var headCheckWindow [][]cid.Cid
		ctx := context.Background()

		api, closer, err := GetFullNodeAPI(repo)
		if err != nil {
			return err
		}
		defer closer()

		if err := WaitForSyncComplete(ctx, api); err != nil {
			log.Fatal(err)
		}

		ch := make(chan [][]cid.Cid, 1)
		aCh := make(chan interface{}, 1)

		go func() {
			for {
				headCheckWindow, err = updateWindow(ctx, api, headCheckWindow, threshold, ch)
				if err != nil {
					log.Fatal(err)
				}
				time.Sleep(interval * time.Second)
			}
		}()

		go func() {
			for {
				result, err := alertHandler(name, aCh)
				if err != nil {
					log.Fatal(err)
				}
				if result != "done" {
					log.Fatal("systemd unit failed to restart:", result)
				}
				log.Info("restarting health agent")
				os.Exit(130)
			}
		}()

		for {
			ok := checkWindow(ch, int(interval))
			if !ok {
				log.Warn("chain head has not updated. Restarting systemd service")
				aCh <- nil
				break
			}
			log.Info("chain head is healthy")
		}
		return nil
	},
}

func checkWindow(ch chan [][]cid.Cid, t int) bool {
	select {
	case window := <-ch:
		var dup int
		windowLen := len(window)
		if windowLen >= t {
		cidWindow:
			for i, cids := range window {
				fmt.Print("yo")
				next := windowLen - 1 - i
				// if array length is different, head is changing
				if next >= 1 && len(window[next]) != len(window[next-1]) {
					break cidWindow
				}
				// if cids are different, head is changing
				for j := range cids {
					if next >= 1 && window[next][j] != window[next-1][j] {
						break cidWindow
					}
				}
				if i < (t - 1) {
					dup++
				}
			}

			if dup == (t - 1) {
				return false
			}
		}
		return true
	}
}

func updateWindow(ctx context.Context, a api.FullNode, w [][]cid.Cid, t int, ch chan [][]cid.Cid) ([][]cid.Cid, error) {
	head, err := a.ChainHead(ctx)
	if err != nil {
		return nil, err
	}

	window := appendCIDsToWindow(w, head.Cids(), t)
	ch <- window
	return window, nil
}

func appendCIDsToWindow(w [][]cid.Cid, c []cid.Cid, t int) [][]cid.Cid {
	offset := len(w) - t + 1
	if offset >= 0 {
		return append(w[offset:], c)
	}
	return append(w, c)
}

func getAPI(path string) (string, http.Header, error) {
	r, err := repo.NewFS(path)
	if err != nil {
		return "", nil, err
	}

	ma, err := r.APIEndpoint()
	if err != nil {
		return "", nil, xerrors.Errorf("failed to get api endpoint: %w", err)
	}
	_, addr, err := manet.DialArgs(ma)
	if err != nil {
		return "", nil, err
	}
	var headers http.Header
	token, err := r.APIToken()
	if err != nil {
		log.Warn("Couldn't load CLI token, capabilities may be limited: %w", err)
	} else {
		headers = http.Header{}
		headers.Add("Authorization", "Bearer "+string(token))
	}

	return "ws://" + addr + "/rpc/v0", headers, nil
}

func GetFullNodeAPI(repo string) (api.FullNode, jsonrpc.ClientCloser, error) {
	addr, headers, err := getAPI(repo)
	if err != nil {
		return nil, nil, err
	}

	return client.NewFullNodeRPC(addr, headers)
}

func WaitForSyncComplete(ctx context.Context, napi api.FullNode) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
			head, err := napi.ChainHead(ctx)
			if err != nil {
				return err
			}

			if time.Now().Unix()-int64(head.MinTimestamp()) < build.BlockDelay {
				return nil
			}
		}
	}
}
