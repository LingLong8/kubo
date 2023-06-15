package _func

import (
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	gopath "path"

	"github.com/ipfs/boxo/coreiface/path"
	"github.com/ipfs/boxo/files"
	cmds "github.com/ipfs/go-ipfs-cmds"
	"github.com/ipfs/kubo/core/commands/cmdenv"
)

var ErrInvalidCompressionLevel = errors.New("compression level must be between 1 and 9")

const (
	outputOptionName           = "output"
	archiveOptionName          = "archive"
	compressOptionName         = "compress"
	compressionLevelOptionName = "compression-level"
)

func Get(req *cmds.Request, env cmds.Environment) error {
	ctx := req.Context
	cmplvl, err := getCompressOptions(req)
	if err != nil {
		return err
	}

	api, err := cmdenv.GetApi(env, req)
	if err != nil {
		return err
	}

	p := path.New(req.Arguments[0])

	file, err := api.Unixfs().Get(ctx, p)
	if err != nil {
		return err
	}

	size, err := file.Size()
	if err != nil {
		return err
	}
	fmt.Printf("file size: %d", size)

	//res.SetLength(uint64(size))

	archive, _ := req.Options[archiveOptionName].(bool)
	reader, err := fileArchive(file, p.String(), archive, cmplvl)
	if err != nil {
		return err
	}
	go func() {
		// We cannot defer a close in the response writer (like we should)
		// Because the cmd framework outsmart us and doesn't call response
		// if the context is over.
		<-ctx.Done()
		reader.Close()
	}()

	//return res.Emit(reader)
	return nil
}

func getCompressOptions(req *cmds.Request) (int, error) {
	cmprs, _ := req.Options[compressOptionName].(bool)
	cmplvl, cmplvlFound := req.Options[compressionLevelOptionName].(int)
	switch {
	case !cmprs:
		return gzip.NoCompression, nil
	case cmprs && !cmplvlFound:
		return gzip.DefaultCompression, nil
	case cmprs && (cmplvl < 1 || cmplvl > 9):
		return gzip.NoCompression, ErrInvalidCompressionLevel
	}
	return cmplvl, nil
}

// DefaultBufSize is the buffer size for gets. for now, 1MiB, which is ~4 blocks.
// TODO: does this need to be configurable?
var DefaultBufSize = 1048576

type identityWriteCloser struct {
	w io.Writer
}

func (i *identityWriteCloser) Write(p []byte) (int, error) {
	return i.w.Write(p)
}

func (i *identityWriteCloser) Close() error {
	return nil
}

func fileArchive(f files.Node, name string, archive bool, compression int) (io.ReadCloser, error) {
	cleaned := gopath.Clean(name)
	_, filename := gopath.Split(cleaned)

	// need to connect a writer to a reader
	piper, pipew := io.Pipe()
	checkErrAndClosePipe := func(err error) bool {
		if err != nil {
			_ = pipew.CloseWithError(err)
			return true
		}
		return false
	}

	// use a buffered writer to parallelize task
	bufw := bufio.NewWriterSize(pipew, DefaultBufSize)

	// compression determines whether to use gzip compression.
	maybeGzw, err := newMaybeGzWriter(bufw, compression)
	if checkErrAndClosePipe(err) {
		return nil, err
	}

	closeGzwAndPipe := func() {
		if err := maybeGzw.Close(); checkErrAndClosePipe(err) {
			return
		}
		if err := bufw.Flush(); checkErrAndClosePipe(err) {
			return
		}
		pipew.Close() // everything seems to be ok.
	}

	if !archive && compression != gzip.NoCompression {
		// the case when the node is a file
		r := files.ToFile(f)
		if r == nil {
			return nil, errors.New("file is not regular")
		}

		go func() {
			if _, err := io.Copy(maybeGzw, r); checkErrAndClosePipe(err) {
				return
			}
			closeGzwAndPipe() // everything seems to be ok
		}()
	} else {
		// the case for 1. archive, and 2. not archived and not compressed, in which tar is used anyway as a transport format

		// construct the tar writer
		w, err := files.NewTarWriter(maybeGzw)
		if checkErrAndClosePipe(err) {
			return nil, err
		}

		go func() {
			// write all the nodes recursively
			if err := w.WriteFile(f, filename); checkErrAndClosePipe(err) {
				return
			}
			w.Close()         // close tar writer
			closeGzwAndPipe() // everything seems to be ok
		}()
	}

	return piper, nil
}

func newMaybeGzWriter(w io.Writer, compression int) (io.WriteCloser, error) {
	if compression != gzip.NoCompression {
		return gzip.NewWriterLevel(w, compression)
	}
	return &identityWriteCloser{w}, nil
}
