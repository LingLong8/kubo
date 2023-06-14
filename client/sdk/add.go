package sdk

import (
	"errors"
	"fmt"
	coreiface "github.com/ipfs/boxo/coreiface"
	"github.com/ipfs/boxo/coreiface/options"
	"github.com/ipfs/boxo/files"
	"github.com/ipfs/boxo/mfs"
	cmds "github.com/ipfs/go-ipfs-cmds"
	ipld "github.com/ipfs/go-ipld-format"
	"github.com/ipfs/kubo/core/commands/cmdenv"
	mh "github.com/multiformats/go-multihash"
	"path"
	gopath "path"
	"strings"
)

const adderOutChanSize = 8

const (
	quietOptionName       = "quiet"
	quieterOptionName     = "quieter"
	silentOptionName      = "silent"
	progressOptionName    = "progress"
	trickleOptionName     = "trickle"
	wrapOptionName        = "wrap-with-directory"
	onlyHashOptionName    = "only-hash"
	chunkerOptionName     = "chunker"
	pinOptionName         = "pin"
	rawLeavesOptionName   = "raw-leaves"
	noCopyOptionName      = "nocopy"
	fstoreCacheOptionName = "fscache"
	cidVersionOptionName  = "cid-version"
	hashOptionName        = "hash"
	inlineOptionName      = "inline"
	inlineLimitOptionName = "inline-limit"
	toFilesOptionName     = "to-files"
)

func Add(req *cmds.Request, env cmds.Environment) error {
	api, err := cmdenv.GetApi(env, req)
	if err != nil {
		return err
	}

	progress, _ := req.Options[progressOptionName].(bool)
	trickle, _ := req.Options[trickleOptionName].(bool)
	wrap, _ := req.Options[wrapOptionName].(bool)
	hash, _ := req.Options[onlyHashOptionName].(bool)
	silent, _ := req.Options[silentOptionName].(bool)
	chunker, _ := req.Options[chunkerOptionName].(string)
	dopin, _ := req.Options[pinOptionName].(bool)
	rawblks, rbset := req.Options[rawLeavesOptionName].(bool)
	nocopy, _ := req.Options[noCopyOptionName].(bool)
	fscache, _ := req.Options[fstoreCacheOptionName].(bool)
	cidVer, cidVerSet := req.Options[cidVersionOptionName].(int)
	hashFunStr, _ := req.Options[hashOptionName].(string)
	inline, _ := req.Options[inlineOptionName].(bool)
	inlineLimit, _ := req.Options[inlineLimitOptionName].(int)
	toFilesStr, toFilesSet := req.Options[toFilesOptionName].(string)

	hashFunCode, ok := mh.Names[strings.ToLower(hashFunStr)]
	if !ok {
		return fmt.Errorf("unrecognized hash function: %q", strings.ToLower(hashFunStr))
	}

	enc, err := cmdenv.GetCidEncoder(req)
	if err != nil {
		return err
	}

	toadd := req.Files
	if wrap {
		toadd = files.NewSliceDirectory([]files.DirEntry{
			files.FileEntry("", req.Files),
		})
	}

	opts := []options.UnixfsAddOption{
		options.Unixfs.Hash(hashFunCode),

		options.Unixfs.Inline(inline),
		options.Unixfs.InlineLimit(inlineLimit),

		options.Unixfs.Chunker(chunker),

		options.Unixfs.Pin(dopin),
		options.Unixfs.HashOnly(hash),
		options.Unixfs.FsCache(fscache),
		options.Unixfs.Nocopy(nocopy),

		options.Unixfs.Progress(progress),
		options.Unixfs.Silent(silent),
	}

	if cidVerSet {
		opts = append(opts, options.Unixfs.CidVersion(cidVer))
	}

	if rbset {
		opts = append(opts, options.Unixfs.RawLeaves(rawblks))
	}

	if trickle {
		opts = append(opts, options.Unixfs.Layout(options.TrickleLayout))
	}

	opts = append(opts, nil) // events option placeholder

	ipfsNode, err := cmdenv.GetNode(env)
	if err != nil {
		return err
	}
	var added int
	var fileAddedToMFS bool
	addit := toadd.Entries()
	for addit.Next() {
		_, dir := addit.Node().(files.Directory)
		errCh := make(chan error, 1)
		events := make(chan interface{}, adderOutChanSize)
		opts[len(opts)-1] = options.Unixfs.Events(events)

		go func() {
			var err error
			defer close(events)
			pathAdded, err := api.Unixfs().Add(req.Context, addit.Node(), opts...)
			if err != nil {
				errCh <- err
				return
			}

			// creating MFS pointers when optional --to-files is set
			if toFilesSet {
				if toFilesStr == "" {
					toFilesStr = "/"
				}
				toFilesDst, err := checkPath(toFilesStr)
				if err != nil {
					errCh <- fmt.Errorf("%s: %w", toFilesOptionName, err)
					return
				}
				dstAsDir := toFilesDst[len(toFilesDst)-1] == '/'

				if dstAsDir {
					mfsNode, err := mfs.Lookup(ipfsNode.FilesRoot, toFilesDst)
					// confirm dst exists
					if err != nil {
						errCh <- fmt.Errorf("%s: MFS destination directory %q does not exist: %w", toFilesOptionName, toFilesDst, err)
						return
					}
					// confirm dst is a dir
					if mfsNode.Type() != mfs.TDir {
						errCh <- fmt.Errorf("%s: MFS destination %q is not a directory", toFilesOptionName, toFilesDst)
						return
					}
					// if MFS destination is a dir, append filename to the dir path
					toFilesDst += path.Base(addit.Name())
				}

				// error if we try to overwrite a preexisting file destination
				if fileAddedToMFS && !dstAsDir {
					errCh <- fmt.Errorf("%s: MFS destination is a file: only one entry can be copied to %q", toFilesOptionName, toFilesDst)
					return
				}

				_, err = mfs.Lookup(ipfsNode.FilesRoot, path.Dir(toFilesDst))
				if err != nil {
					errCh <- fmt.Errorf("%s: MFS destination parent %q %q does not exist: %w", toFilesOptionName, toFilesDst, path.Dir(toFilesDst), err)
					return
				}

				var nodeAdded ipld.Node
				nodeAdded, err = api.Dag().Get(req.Context, pathAdded.Cid())
				if err != nil {
					errCh <- err
					return
				}
				err = mfs.PutNode(ipfsNode.FilesRoot, toFilesDst, nodeAdded)
				if err != nil {
					errCh <- fmt.Errorf("%s: cannot put node in path %q: %w", toFilesOptionName, toFilesDst, err)
					return
				}
				fileAddedToMFS = true
			}
			errCh <- err
		}()

		for event := range events {
			output, ok := event.(*coreiface.AddEvent)
			if !ok {
				return errors.New("unknown event type")
			}

			h := ""
			if output.Path != nil {
				h = enc.Encode(output.Path.Cid())
			}

			if !dir && addit.Name() != "" {
				output.Name = addit.Name()
			} else {
				output.Name = path.Join(addit.Name(), output.Name)
			}

			addEvent := AddEvent{
				Name:  output.Name,
				Hash:  h,
				Bytes: output.Bytes,
				Size:  output.Size,
			}
			fmt.Printf("AddEvent: %+v", addEvent)
		}

		if err := <-errCh; err != nil {
			return err
		}
		added++
	}

	if addit.Err() != nil {
		return addit.Err()
	}

	if added == 0 {
		return fmt.Errorf("expected a file argument")
	}

	return nil
}

type AddEvent struct {
	Name  string
	Hash  string `json:",omitempty"`
	Bytes int64  `json:",omitempty"`
	Size  string `json:",omitempty"`
}

func checkPath(p string) (string, error) {
	if len(p) == 0 {
		return "", fmt.Errorf("paths must not be empty")
	}

	if p[0] != '/' {
		return "", fmt.Errorf("paths must start with a leading slash")
	}

	cleaned := gopath.Clean(p)
	if p[len(p)-1] == '/' && p != "/" {
		cleaned += "/"
	}
	return cleaned, nil
}
