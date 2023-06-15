package _func

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ipfs/boxo/coreiface/options"
	"github.com/ipfs/boxo/files"
	"github.com/ipfs/boxo/ipld/unixfs"
	"github.com/ipfs/boxo/path"
	cmds "github.com/ipfs/go-ipfs-cmds"
	"github.com/ipfs/kubo/assets"
	oldcmds "github.com/ipfs/kubo/commands"
	"github.com/ipfs/kubo/config"
	"github.com/ipfs/kubo/core"
	"github.com/ipfs/kubo/repo/fsrepo"
)

const (
	algorithmDefault    = options.Ed25519Key
	algorithmOptionName = "algorithm"
	bitsOptionName      = "bits"
	emptyRepoDefault    = true
	emptyRepoOptionName = "empty-repo"
	profileOptionName   = "profile"
)

// nolint
var errRepoExists = errors.New(`ipfs configuration file already exists!
Reinitializing would overwrite your keys
`)

func initConf(req *cmds.Request, env cmds.Environment) error {
	cctx := env.(*oldcmds.Context)
	empty, _ := req.Options[emptyRepoOptionName].(bool)
	algorithm, _ := req.Options[algorithmOptionName].(string)
	nBitsForKeypair, nBitsGiven := req.Options[bitsOptionName].(int)

	var conf *config.Config

	f := req.Files
	if f != nil {
		it := req.Files.Entries()
		if !it.Next() {
			if it.Err() != nil {
				return it.Err()
			}
			return fmt.Errorf("file argument was nil")
		}
		file := files.FileFromEntry(it)
		if file == nil {
			return fmt.Errorf("expected a regular file")
		}

		conf = &config.Config{}
		if err := json.NewDecoder(file).Decode(conf); err != nil {
			return err
		}
	}

	if conf == nil {
		var err error
		var identity config.Identity
		if nBitsGiven {
			identity, err = config.CreateIdentity(os.Stdout, []options.KeyGenerateOption{
				options.Key.Size(nBitsForKeypair),
				options.Key.Type(algorithm),
			})
		} else {
			identity, err = config.CreateIdentity(os.Stdout, []options.KeyGenerateOption{
				options.Key.Type(algorithm),
			})
		}
		if err != nil {
			return err
		}
		conf, err = config.InitWithIdentity(identity)
		if err != nil {
			return err
		}
	}

	profiles, _ := req.Options[profileOptionName].(string)
	return doInit(os.Stdout, cctx.ConfigRoot, empty, profiles, conf)
}

func doInit(out io.Writer, repoRoot string, empty bool, confProfiles string, conf *config.Config) error {
	if _, err := fmt.Fprintf(out, "initializing IPFS node at %s\n", repoRoot); err != nil {
		return err
	}

	if err := checkWritable(repoRoot); err != nil {
		return err
	}

	if fsrepo.IsInitialized(repoRoot) {
		return errRepoExists
	}

	if err := applyProfiles(conf, confProfiles); err != nil {
		return err
	}

	if err := fsrepo.Init(repoRoot, conf); err != nil {
		return err
	}

	if !empty {
		if err := addDefaultAssets(out, repoRoot); err != nil {
			return err
		}
	}

	return initializeIpnsKeyspace(repoRoot)
}

func checkWritable(dir string) error {
	_, err := os.Stat(dir)
	if err == nil {
		// dir exists, make sure we can write to it
		testfile := filepath.Join(dir, "test")
		fi, err := os.Create(testfile)
		if err != nil {
			if os.IsPermission(err) {
				return fmt.Errorf("%s is not writeable by the current user", dir)
			}
			return fmt.Errorf("unexpected error while checking writeablility of repo root: %s", err)
		}
		fi.Close()
		return os.Remove(testfile)
	}

	if os.IsNotExist(err) {
		// dir doesn't exist, check that we can create it
		return os.Mkdir(dir, 0775)
	}

	if os.IsPermission(err) {
		return fmt.Errorf("cannot write to %s, incorrect permissions", err)
	}

	return err
}

func applyProfiles(conf *config.Config, profiles string) error {
	if profiles == "" {
		return nil
	}

	for _, profile := range strings.Split(profiles, ",") {
		transformer, ok := config.Profiles[profile]
		if !ok {
			return fmt.Errorf("invalid configuration profile: %s", profile)
		}

		if err := transformer.Transform(conf); err != nil {
			return err
		}
	}
	return nil
}

func addDefaultAssets(out io.Writer, repoRoot string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r, err := fsrepo.Open(repoRoot)
	if err != nil { // NB: repo is owned by the node
		return err
	}

	nd, err := core.NewNode(ctx, &core.BuildCfg{Repo: r})
	if err != nil {
		return err
	}
	defer nd.Close()

	dkey, err := assets.SeedInitDocs(nd)
	if err != nil {
		return fmt.Errorf("init: seeding init docs failed: %s", err)
	}
	//log.Debugf("init: seeded init docs %s", dkey)

	if _, err = fmt.Fprintf(out, "to get started, enter:\n"); err != nil {
		return err
	}

	_, err = fmt.Fprintf(out, "\n\tipfs cat /ipfs/%s/readme\n\n", dkey)
	return err
}

func initializeIpnsKeyspace(repoRoot string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r, err := fsrepo.Open(repoRoot)
	if err != nil { // NB: repo is owned by the node
		return err
	}

	nd, err := core.NewNode(ctx, &core.BuildCfg{Repo: r})
	if err != nil {
		return err
	}
	defer nd.Close()

	emptyDir := unixfs.EmptyDirNode()

	// pin recursively because this might already be pinned
	// and doing a direct pin would throw an error in that case
	err = nd.Pinning.Pin(ctx, emptyDir, true)
	if err != nil {
		return err
	}

	err = nd.Pinning.Flush(ctx)
	if err != nil {
		return err
	}

	return nd.Namesys.Publish(ctx, nd.PrivateKey, path.FromCid(emptyDir.Cid()))
}