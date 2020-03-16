package command

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strings"

	"github.com/hashicorp/go-multierror"
	"github.com/urfave/cli/v2"

	errorpkg "github.com/peak/s5cmd/error"
	"github.com/peak/s5cmd/log"
	"github.com/peak/s5cmd/objurl"
	"github.com/peak/s5cmd/parallel"
	"github.com/peak/s5cmd/storage"
)

// shouldOverrideFunc is a helper closure for shouldOverride function.
type shouldOverrideFunc func(dst *objurl.ObjectURL) error

var copyCommandFlags = []cli.Flag{
	&cli.BoolFlag{Name: "no-clobber", Aliases: []string{"n"}},
	&cli.BoolFlag{Name: "if-size-differ", Aliases: []string{"s"}},
	&cli.BoolFlag{Name: "if-source-newer", Aliases: []string{"u"}},
	&cli.BoolFlag{Name: "parents"},
	&cli.BoolFlag{Name: "recursive", Aliases: []string{"R"}},
	&cli.StringFlag{Name: "storage-class"},
}

var CopyCommand = &cli.Command{
	Name:     "cp",
	HelpName: "copy",
	Usage:    "TODO",
	Flags:    copyCommandFlags,
	Before: func(c *cli.Context) error {
		if c.Args().Len() != 2 {
			return fmt.Errorf("expected source and destination arguments")
		}

		dst, err := objurl.New(c.Args().Get(1))
		if err != nil {
			return err
		}

		if dst.HasGlob() {
			return fmt.Errorf("target %q can not contain glob characters", dst)
		}

		return nil
	},
	Action: func(c *cli.Context) error {
		noClobber := c.Bool("no-clobber")
		ifSizeDiffer := c.Bool("if-size-differ")
		ifSourceNewer := c.Bool("if-source-newer")
		recursive := c.Bool("recursive")
		parents := c.Bool("parents")
		storageClass := storage.LookupClass(c.String("storage-class"))

		return Copy(
			c.Context,
			c.Args().Get(0),
			c.Args().Get(1),
			c.Command.Name,
			givenCommand(c),
			false, // don't delete source
			// flags
			noClobber,
			ifSizeDiffer,
			ifSourceNewer,
			recursive,
			parents,
			storageClass,
		)
	},
}

// TODO(ig): this function could be in the storage layer.
func expandSource(
	ctx context.Context,
	src *objurl.ObjectURL,
	isRecursive bool,
) (<-chan *storage.Object, error) {
	client, err := storage.NewClient(src)
	if err != nil {
		return nil, err
	}

	var isDir bool
	if !src.HasGlob() && !src.IsRemote() {
		obj, err := client.Stat(ctx, src)
		if err != nil {
			return nil, err
		}
		isDir = obj.Type.IsDir()
	}

	if src.HasGlob() || isDir {
		return client.List(ctx, src, isRecursive, storage.ListAllItems), nil
	}

	ch := make(chan *storage.Object, 1)
	ch <- &storage.Object{URL: src}
	close(ch)
	return ch, nil
}

func Copy(
	ctx context.Context,
	src string,
	dst string,
	op string,
	fullCommand string,
	deleteSource bool,
	// flags
	noClobber bool,
	ifSizeDiffer bool,
	ifSourceNewer bool,
	recursive bool,
	parents bool,
	storageClass storage.StorageClass,
) error {
	srcurl, err := objurl.New(src)
	if err != nil {
		return err
	}

	dsturl, err := objurl.New(dst)
	if err != nil {
		return err
	}

	// set recursive=true for local->remote copy operations. this
	// is required for backwards compatibility.
	recursive = recursive || (!srcurl.IsRemote() && dsturl.IsRemote())

	objch, err := expandSource(ctx, srcurl, recursive)
	if err != nil {
		return err
	}

	waiter := parallel.NewWaiter()

	var merror error
	go func() {
		for err := range waiter.Err() {
			merror = multierror.Append(merror, err)
		}
	}()

	for object := range objch {
		if err := object.Err; err != nil {
			printError(fullCommand, op, err)
			continue
		}

		if object.Type.IsDir() {
			continue
		}

		src := object.URL

		shouldOverrideFunc := func(dst *objurl.ObjectURL) error {
			return shouldOverride(
				ctx,
				src,
				dst,
				noClobber,
				ifSizeDiffer,
				ifSourceNewer,
			)
		}

		var task parallel.Task

		switch {
		case srcurl.Type == dsturl.Type: // local->local or remote->remote
			task = func() error {
				dsturl, err := prepareCopyDestination(ctx, srcurl, src, dsturl, parents)
				if err != nil {
					return err
				}

				err = doCopy(
					ctx,
					src,
					dsturl,
					op,
					deleteSource,
					shouldOverrideFunc,
					// flags
					parents,
					storageClass,
				)
				if err != nil {
					return &errorpkg.Error{
						Op:  op,
						Src: src,
						Dst: dsturl,
						Err: err,
					}
				}
				return nil
			}
		case srcurl.IsRemote(): // remote->local
			task = func() error {
				dsturl, err := prepareDownloadDestination(ctx, srcurl, src, dsturl, parents)
				if err != nil {
					return err
				}

				err = doDownload(
					ctx,
					src,
					dsturl,
					op,
					deleteSource,
					shouldOverrideFunc,
					// flags
					parents,
				)

				if err != nil {
					return &errorpkg.Error{
						Op:  op,
						Src: src,
						Dst: dsturl,
						Err: err,
					}
				}
				return nil
			}
		case dsturl.IsRemote(): // local->remote
			task = func() error {
				err := doUpload(
					ctx,
					src,
					dsturl,
					op,
					deleteSource,
					shouldOverrideFunc,
					// flags
					parents,
					storageClass,
				)
				if err != nil {
					return &errorpkg.Error{
						Op:  op,
						Src: src,
						Dst: dsturl,
						Err: err,
					}
				}
				return nil
			}
		default:
			panic("unexpected src-dst pair")
		}

		parallel.Run(task, waiter)
	}

	waiter.Wait()

	return merror
}

// doDownload is used to fetch a remote object and save as a local object.
func doDownload(
	ctx context.Context,
	src *objurl.ObjectURL,
	dst *objurl.ObjectURL,
	op string,
	deleteSource bool,
	shouldOverride shouldOverrideFunc,
	// flags
	parents bool,
) error {
	srcClient, err := storage.NewClient(src)
	if err != nil {
		return err
	}

	dstClient, err := storage.NewClient(dst)
	if err != nil {
		return err
	}

	err = shouldOverride(dst)
	if err != nil {
		// FIXME(ig): rename
		if isWarning(err) {
			printDebug(op, src, dst, err)
			return nil
		}
		return err
	}

	f, err := os.Create(dst.Absolute())
	if err != nil {
		return err
	}
	defer f.Close()

	size, err := srcClient.Get(ctx, src, f)
	if err != nil {
		err = dstClient.Delete(ctx, dst)
	} else if deleteSource {
		err = srcClient.Delete(ctx, src)
	}

	if err != nil {
		return err
	}

	msg := log.InfoMessage{
		Operation:   op,
		Source:      src,
		Destination: dst,
		Object: &storage.Object{
			Size: size,
		},
	}
	log.Info(msg)

	return nil
}

func doUpload(
	ctx context.Context,
	src *objurl.ObjectURL,
	dst *objurl.ObjectURL,
	op string,
	deleteSource bool,
	shouldOverride shouldOverrideFunc,
	// flags
	parents bool,
	storageClass storage.StorageClass,
) error {
	// TODO(ig): use storage abstraction
	f, err := os.Open(src.Absolute())
	if err != nil {
		return err
	}
	defer f.Close()

	objname := src.Base()
	if parents {
		objname = src.Relative()
	}

	dst = dst.Join(objname)

	err = shouldOverride(dst)
	if err != nil {
		if isWarning(err) {
			printDebug(op, src, dst, err)
			return nil
		}
		return err
	}

	dstClient, err := storage.NewClient(dst)
	if err != nil {
		return err
	}

	metadata := map[string]string{
		"StorageClass": string(storageClass),
		"ContentType":  guessContentType(f),
	}

	err = dstClient.Put(
		ctx,
		f,
		dst,
		metadata,
	)
	if err != nil {
		return err
	}

	srcClient, err := storage.NewClient(src)
	if err != nil {
		return err
	}

	obj, _ := srcClient.Stat(ctx, src)
	size := obj.Size

	if deleteSource {
		if err := srcClient.Delete(ctx, src); err != nil {
			return err
		}
	}

	msg := log.InfoMessage{
		Operation:   op,
		Source:      src,
		Destination: dst,
		Object: &storage.Object{
			Size:         size,
			StorageClass: storageClass,
		},
	}
	log.Info(msg)

	return nil
}

func doCopy(
	ctx context.Context,
	src *objurl.ObjectURL,
	dst *objurl.ObjectURL,
	op string,
	deleteSource bool,
	shouldOverride shouldOverrideFunc,
	// flags
	parents bool,
	storageClass storage.StorageClass,
) error {
	srcClient, err := storage.NewClient(src)
	if err != nil {
		return err
	}

	metadata := map[string]string{
		"StorageClass": string(storageClass),
	}

	err = shouldOverride(dst)
	if err != nil {
		if isWarning(err) {
			printDebug(op, src, dst, err)
			return nil
		}
		return err
	}

	err = srcClient.Copy(
		ctx,
		src,
		dst,
		metadata,
	)
	if err != nil {
		return err
	}

	if deleteSource {
		if err := srcClient.Delete(ctx, src); err != nil {
			return err
		}
	}

	msg := log.InfoMessage{
		Operation:   op,
		Source:      src,
		Destination: dst,
		Object: &storage.Object{
			URL:          dst,
			StorageClass: storage.StorageClass(storageClass),
		},
	}
	log.Info(msg)

	return nil
}

func guessContentType(rs io.ReadSeeker) string {
	defer rs.Seek(0, io.SeekStart)

	const bufsize = 512
	buf, err := ioutil.ReadAll(io.LimitReader(rs, bufsize))
	if err != nil {
		return ""
	}

	return http.DetectContentType(buf)
}

func givenCommand(c *cli.Context) string {
	return fmt.Sprintf("%v %v", c.Command.FullName(), strings.Join(c.Args().Slice(), " "))
}

// prepareCopyDestination will return a new destination URL for local->local
// and remote->remote copy operations.
func prepareCopyDestination(
	ctx context.Context,
	originalSrc *objurl.ObjectURL,
	src *objurl.ObjectURL,
	dst *objurl.ObjectURL,
	parents bool,
) (*objurl.ObjectURL, error) {
	objname := src.Base()
	if parents {
		objname = src.Relative()
	}

	// For remote->remote copy operations, treat <dst> as prefix if it has "/"
	// suffix.
	if dst.IsRemote() {
		if strings.HasSuffix(dst.Path, "/") {
			dst = dst.Join(objname)
		}
		return dst, nil
	}

	client, err := storage.NewClient(dst)
	if err != nil {
		return nil, err
	}

	// For local->local copy operations, we can safely stat <dst> to check if
	// it is a file or a directory.
	obj, err := client.Stat(ctx, dst)
	if err != nil && err != storage.ErrGivenObjectNotFound {
		return nil, err
	}

	// Absolute <src> path is given. Use given <dst> and local copy operation
	// will create missing directories if <dst> has one.
	if !originalSrc.HasGlob() {
		return dst, nil
	}

	// For local->local copy operations, if <src> has glob, <dst> is expected
	// to be a directory. As always, local copy operation will create missing
	// directories if <dst> has one.
	if obj != nil && !obj.Type.IsDir() {
		return nil, fmt.Errorf("destination argument is expected to be a directory")
	}

	return dst.Join(objname), nil
}

// prepareDownloadDestination will return a new destination URL for
// remote->local and remote->remote copy operations.
func prepareDownloadDestination(
	ctx context.Context,
	originalSrc *objurl.ObjectURL,
	src *objurl.ObjectURL,
	dst *objurl.ObjectURL,
	parents bool,
) (*objurl.ObjectURL, error) {
	objname := src.Base()
	if parents {
		objname = src.Relative()
	}

	if originalSrc.HasGlob() {
		os.MkdirAll(dst.Absolute(), os.ModePerm)
	}

	client, err := storage.NewClient(dst)
	if err != nil {
		return nil, err
	}

	obj, err := client.Stat(ctx, dst)
	if err != nil && err != storage.ErrGivenObjectNotFound {
		return nil, err
	}

	if parents {
		if obj != nil && !obj.Type.IsDir() {
			return nil, fmt.Errorf("destination argument is expected to be a directory")
		}
		dst = dst.Join(objname)
		os.MkdirAll(dst.Dir(), os.ModePerm)
	}

	if err == storage.ErrGivenObjectNotFound {
		os.MkdirAll(dst.Dir(), os.ModePerm)
		if strings.HasSuffix(dst.Absolute(), "/") {
			dst = dst.Join(objname)
		}
	} else {
		if obj.Type.IsDir() {
			dst = obj.URL.Join(objname)
		}
	}

	return dst, nil
}
