package env

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	u "github.com/ipfs/boxo/util"
	cmds "github.com/ipfs/go-ipfs-cmds"
	cmdhttp "github.com/ipfs/go-ipfs-cmds/http"
	logging "github.com/ipfs/go-log"
	oldcmds "github.com/ipfs/kubo/commands"
	"github.com/ipfs/kubo/core"
	corecmds "github.com/ipfs/kubo/core/commands"
	"github.com/ipfs/kubo/core/corehttp"
	"github.com/ipfs/kubo/plugin/loader"
	"github.com/ipfs/kubo/repo"
	"github.com/ipfs/kubo/repo/fsrepo"
	ma "github.com/multiformats/go-multiaddr"
	madns "github.com/multiformats/go-multiaddr-dns"
	manet "github.com/multiformats/go-multiaddr/net"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var log = logging.Logger("sdk/ipfs")
var tracer trace.Tracer // todo 该组件应该可以移除

// declared as a var for testing purposes
var dnsResolver = madns.DefaultResolver

func buildEnv(ctx context.Context, req *cmds.Request) (cmds.Environment, error) {
	checkDebug(req)
	repoPath, err := getRepoPath(req)
	if err != nil {
		return nil, err
	}
	log.Debugf("config path is %s", repoPath)

	plugins, err := loadPlugins(repoPath)
	if err != nil {
		return nil, err
	}

	// this sets up the function that will initialize the node
	// this is so that we can construct the node lazily.
	return &oldcmds.Context{
		ConfigRoot: repoPath,
		ReqLog:     &oldcmds.ReqLog{},
		Plugins:    plugins,
		ConstructNode: func() (n *core.IpfsNode, err error) {
			if req == nil {
				return nil, errors.New("constructing node without a request")
			}

			r, err := fsrepo.Open(repoPath)
			if err != nil { // repo is owned by the node
				return nil, err
			}

			// ok everything is good. set it on the invocation (for ownership)
			// and return it.
			n, err = core.NewNode(ctx, &core.BuildCfg{
				Repo: r,
			})
			if err != nil {
				return nil, err
			}

			return n, nil
		},
	}, nil
}

func checkDebug(req *cmds.Request) {
	// check if user wants to debug. option OR env var.
	debug, _ := req.Options["debug"].(bool)
	if debug || os.Getenv("IPFS_LOGGING") == "debug" {
		u.Debug = true
		logging.SetDebugLogging()
	}
	if u.GetenvBool("DEBUG") {
		u.Debug = true
	}
}

func getRepoPath(req *cmds.Request) (string, error) {
	repoOpt, found := req.Options[corecmds.RepoDirOption].(string)
	if found && repoOpt != "" {
		return repoOpt, nil
	}

	repoPath, err := fsrepo.BestKnownPath()
	if err != nil {
		return "", err
	}
	return repoPath, nil
}

func loadPlugins(repoPath string) (*loader.PluginLoader, error) {
	plugins, err := loader.NewPluginLoader(repoPath)
	if err != nil {
		return nil, fmt.Errorf("error loading plugins: %s", err)
	}

	if err := plugins.Initialize(); err != nil {
		return nil, fmt.Errorf("error initializing plugins: %s", err)
	}

	if err := plugins.Inject(); err != nil {
		return nil, fmt.Errorf("error initializing plugins: %s", err)
	}
	return plugins, nil
}

func makeExecutor(req *cmds.Request, env interface{}) (cmds.Executor, error) {
	exe := tracingWrappedExecutor{cmds.NewExecutor(req.Root)}
	cctx := env.(*oldcmds.Context)

	// Check if the command is disabled.
	if req.Command.NoLocal && req.Command.NoRemote {
		return nil, fmt.Errorf("command disabled: %v", req.Path)
	}

	// Can we just run this locally?
	if !req.Command.NoLocal {
		if doesNotUseRepo, ok := corecmds.GetDoesNotUseRepo(req.Command.Extra); doesNotUseRepo && ok {
			return exe, nil
		}
	}

	// Get the API option from the commandline.
	apiAddr, err := apiAddrOption(req)
	if err != nil {
		return nil, err
	}

	// Require that the command be run on the daemon when the API flag is
	// passed (unless we're trying to _run_ the daemon).
	daemonRequested := apiAddr != nil && true // req.Command != daemonCmd

	// Run this on the client if required.
	if req.Command.NoRemote {
		if daemonRequested {
			// User requested that the command be run on the daemon but we can't.
			// NOTE: We drop this check for the `ipfs daemon` command.
			return nil, errors.New("api flag specified but command cannot be run on the daemon")
		}
		return exe, nil
	}

	// Finally, look in the repo for an API file.
	if apiAddr == nil {
		var err error
		apiAddr, err = fsrepo.APIAddr(cctx.ConfigRoot)
		switch err {
		case nil, repo.ErrApiNotRunning:
		default:
			return nil, err
		}
	}

	// Still no api specified? Run it on the client or fail.
	if apiAddr == nil {
		if req.Command.NoLocal {
			return nil, fmt.Errorf("command must be run on the daemon: %v", req.Path)
		}
		return exe, nil
	}

	// Resolve the API addr.
	apiAddr, err = resolveAddr(req.Context, apiAddr)
	if err != nil {
		return nil, err
	}
	network, host, err := manet.DialArgs(apiAddr)
	if err != nil {
		return nil, err
	}

	// Construct the executor.
	opts := []cmdhttp.ClientOpt{
		cmdhttp.ClientWithAPIPrefix(corehttp.APIPath),
	}

	// Fallback on a local executor if we (a) have a repo and (b) aren't
	// forcing a daemon.
	if !daemonRequested && fsrepo.IsInitialized(cctx.ConfigRoot) {
		opts = append(opts, cmdhttp.ClientWithFallback(exe))
	}

	var tpt http.RoundTripper
	switch network {
	case "tcp", "tcp4", "tcp6":
		tpt = http.DefaultTransport
	case "unix":
		path := host
		host = "unix"
		tpt = &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", path)
			},
		}
	default:
		return nil, fmt.Errorf("unsupported API address: %s", apiAddr)
	}
	opts = append(opts, cmdhttp.ClientWithHTTPClient(&http.Client{
		Transport: otelhttp.NewTransport(tpt),
	}))

	return tracingWrappedExecutor{cmdhttp.NewClient(host, opts...)}, nil
}

type tracingWrappedExecutor struct {
	exec cmds.Executor
}

func (twe tracingWrappedExecutor) Execute(req *cmds.Request, re cmds.ResponseEmitter, env cmds.Environment) error {
	ctx, span := tracer.Start(req.Context, "cmds."+strings.Join(req.Path, "."), trace.WithAttributes(attribute.StringSlice("Arguments", req.Arguments)))
	defer span.End()
	req.Context = ctx

	err := twe.exec.Execute(req, re, env)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

func apiAddrOption(req *cmds.Request) (ma.Multiaddr, error) {
	apiAddrStr, apiSpecified := req.Options[corecmds.ApiOption].(string)
	if !apiSpecified {
		return nil, nil
	}
	return ma.NewMultiaddr(apiAddrStr)
}

func resolveAddr(ctx context.Context, addr ma.Multiaddr) (ma.Multiaddr, error) {
	ctx, cancelFunc := context.WithTimeout(ctx, 10*time.Second)
	defer cancelFunc()

	addrs, err := dnsResolver.Resolve(ctx, addr)
	if err != nil {
		return nil, err
	}

	if len(addrs) == 0 {
		return nil, errors.New("non-resolvable API endpoint")
	}

	return addrs[0], nil
}
