package devnet

import (
	"context"
	"errors"
	"fmt"
	"github.com/ledgerwatch/erigon/cmd/utils"
	"math/big"
	"net"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/ledgerwatch/erigon-lib/common/dbg"
	"github.com/ledgerwatch/erigon/cmd/devnet/args"
	"github.com/ledgerwatch/erigon/cmd/devnet/devnetutils"
	"github.com/ledgerwatch/erigon/cmd/devnet/requests"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/params"
	erigonapp "github.com/ledgerwatch/erigon/turbo/app"
	erigoncli "github.com/ledgerwatch/erigon/turbo/cli"
	"github.com/ledgerwatch/log/v3"
	"github.com/urfave/cli/v2"
)

type Network struct {
	DataDir            string
	Chain              string
	Logger             log.Logger
	BasePort           int
	BasePrivateApiAddr string
	BaseRPCHost        string
	BaseRPCPort        int
	Snapshots          bool
	Nodes              []Node
	Services           []Service
	Alloc              types.GenesisAlloc
	BorStateSyncDelay  time.Duration
	BorPeriod          time.Duration
	BorMinBlockSize    int
	BorWithMilestones  *bool
	wg                 sync.WaitGroup
	peers              []string
	namedNodes         map[string]Node
}

func (nw *Network) ChainID() *big.Int {
	if len(nw.Nodes) > 0 {
		return nw.Nodes[0].ChainID()
	}

	return &big.Int{}
}

// Start starts the process for multiple erigon nodes running on the dev chain
func (nw *Network) Start(ctx context.Context) error {
	for _, service := range nw.Services {
		if err := service.Start(ctx); err != nil {
			nw.Stop()
			return err
		}
	}

	baseNode := args.Node{
		DataDir:        nw.DataDir,
		Chain:          nw.Chain,
		Port:           nw.BasePort,
		HttpPort:       nw.BaseRPCPort,
		PrivateApiAddr: nw.BasePrivateApiAddr,
		Snapshots:      nw.Snapshots,
	}

	if nw.BorWithMilestones != nil {
		baseNode.WithHeimdallMilestones = *nw.BorWithMilestones
	} else {
		baseNode.WithHeimdallMilestones = utils.WithHeimdallMilestones.Value
	}

	cliCtx := CliContext(ctx)

	metricsEnabled := cliCtx.Bool("metrics")
	metricsNode := cliCtx.Int("metrics.node")
	nw.namedNodes = map[string]Node{}

	for i, nodeConfig := range nw.Nodes {
		{
			base := baseNode
			if metricsEnabled && metricsNode == i {
				base.Metrics = true
				base.MetricsPort = cliCtx.Int("metrics.port")
			}
			base.StaticPeers = strings.Join(nw.peers, ",")

			argsObj, err := nodeConfig.Configure(base, i)
			if err != nil {
				nw.Stop()
				return err
			}

			nodePort := nodeConfig.GetHttpPort()
			nodeAddr := fmt.Sprintf("%s:%d", nw.BaseRPCHost, nodePort)

			node, err := nw.createNode(nodeAddr, argsObj)
			if err != nil {
				nw.Stop()
				return err
			}

			nw.Nodes[i] = node
			nw.namedNodes[node.Name()] = node
			nw.peers = append(nw.peers, nodeConfig.GetEnodeURL())

			for _, service := range nw.Services {
				service.NodeCreated(ctx, node)
			}
		}
	}

	for _, node := range nw.Nodes {
		err := nw.startNode(node)
		if err != nil {
			nw.Stop()
			return err
		}

		for _, service := range nw.Services {
			service.NodeStarted(ctx, node)
		}
	}

	return nil
}

var blockProducerFunds = (&big.Int{}).Mul(big.NewInt(1000), big.NewInt(params.Ether))

func (nw *Network) createNode(nodeAddr string, cfg interface{}) (Node, error) {
	n := &node{
		sync.Mutex{},
		requests.NewRequestGenerator(nodeAddr, nw.Logger),
		cfg,
		&nw.wg,
		nw,
		make(chan error),
		nil,
		nil,
		nil,
	}

	if n.IsBlockProducer() {
		if nw.Alloc == nil {
			nw.Alloc = types.GenesisAlloc{
				n.Account().Address: types.GenesisAccount{Balance: blockProducerFunds},
			}
		} else {
			nw.Alloc[n.Account().Address] = types.GenesisAccount{Balance: blockProducerFunds}
		}
	}

	return n, nil
}

func copyFlags(flags []cli.Flag) []cli.Flag {
	copies := make([]cli.Flag, len(flags))

	for i, flag := range flags {
		flagValue := reflect.ValueOf(flag).Elem()
		copyValue := reflect.New(flagValue.Type()).Elem()

		for f := 0; f < flagValue.NumField(); f++ {
			if flagValue.Type().Field(f).PkgPath == "" {
				copyValue.Field(f).Set(flagValue.Field(f))
			}
		}

		copies[i] = copyValue.Addr().Interface().(cli.Flag)
	}

	return copies
}

// startNode starts an erigon node on the dev chain
func (nw *Network) startNode(n Node) error {
	nw.wg.Add(1)

	node := n.(*node)

	args, err := args.AsArgs(node.args)
	if err != nil {
		return err
	}

	go func() {
		nw.Logger.Info("Running node", "name", node.Name(), "args", args)

		// catch any errors and avoid panics if an error occurs
		defer func() {
			panicResult := recover()
			if panicResult == nil {
				return
			}

			nw.Logger.Error("catch panic", "node", node.Name(), "err", panicResult, "stack", dbg.Stack())
			nw.Stop()
			os.Exit(1)
		}()

		// cli flags are not thread safe and assume only one copy of a flag
		// variable is needed per process - which does not work here
		app := erigonapp.MakeApp(node.Name(), node.run, copyFlags(erigoncli.DefaultFlags))

		if err := app.Run(args); err != nil {
			nw.Logger.Warn("App run returned error", "node", node.Name(), "err", err)
		}
	}()

	if err = <-node.startErr; err != nil {
		return err
	}

	return nil
}

func isConnectionError(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return opErr.Op == "dial"
	}
	return false
}

// getEnode returns the enode of the netowrk node
func getEnode(n Node) (string, error) {
	reqCount := 0

	for {
		nodeInfo, err := n.AdminNodeInfo()

		if err != nil {
			if r, ok := n.(*node); ok {
				if !r.running() {
					return "", err
				}
			}

			if isConnectionError(err) && (reqCount < 10) {
				reqCount++
				time.Sleep(time.Duration(devnetutils.RandomInt(5)) * time.Second)
				continue
			}

			return "", err
		}

		enode, err := devnetutils.UniqueIDFromEnode(nodeInfo.Enode)

		if err != nil {
			return "", err
		}

		return enode, nil
	}
}

func (nw *Network) Stop() {
	type stoppable interface {
		Stop()
		running() bool
	}

	for i, n := range nw.Nodes {
		if stoppable, ok := n.(stoppable); ok && stoppable.running() {
			nw.Logger.Info("Stopping", "node", i)
			go stoppable.Stop()
		}
	}

	nw.Logger.Info("Waiting for nodes to stop")
	nw.Wait()

	nw.Logger.Info("Stopping services")
	for _, service := range nw.Services {
		service.Stop()
	}

	// TODO should we wait for services
}

func (nw *Network) Wait() {
	nw.wg.Wait()
}

func (nw *Network) FirstNode() Node {
	return nw.Nodes[0]
}

func (nw *Network) SelectNode(ctx context.Context, selector interface{}) Node {
	switch selector := selector.(type) {
	case int:
		if selector < len(nw.Nodes) {
			return nw.Nodes[selector]
		}
	case NodeSelector:
		for _, node := range nw.Nodes {
			if selector.Test(ctx, node) {
				return node
			}
		}
	}

	return nil
}

func (nw *Network) BlockProducers() []Node {
	var blockProducers []Node

	for _, node := range nw.Nodes {
		if node.IsBlockProducer() {
			blockProducers = append(blockProducers, node)
		}
	}

	return blockProducers
}

func (nw *Network) NonBlockProducers() []Node {
	var nonBlockProducers []Node

	for _, node := range nw.Nodes {
		if !node.IsBlockProducer() {
			nonBlockProducers = append(nonBlockProducers, node)
		}
	}

	return nonBlockProducers
}
