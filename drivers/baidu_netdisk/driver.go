package baidu_netdisk

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	stdpath "path"
	"strconv"
	"time"

	"github.com/alist-org/alist/v3/drivers/base"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/errgroup"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/avast/retry-go"
	log "github.com/sirupsen/logrus"
)

type BaiduNetdisk struct {
	model.Storage
	Addition

	uploadThread int
}

const BaiduFileAPI = "https://d.pcs.baidu.com/rest/2.0/pcs/superfile2"
const DefaultSliceSize int64 = 4 * 1024 * 1024

func (d *BaiduNetdisk) Config() driver.Config {
	return config
}

func (d *BaiduNetdisk) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *BaiduNetdisk) Init(ctx context.Context) error {
	d.uploadThread, _ = strconv.Atoi(d.UploadThread)
	if d.uploadThread < 1 || d.uploadThread > 32 {
		d.uploadThread, d.UploadThread = 3, "3"
	}
	res, err := d.get("/xpan/nas", map[string]string{
		"method": "uinfo",
	}, nil)
	log.Debugf("[baidu] get uinfo: %s", string(res))
	return err
}

func (d *BaiduNetdisk) Drop(ctx context.Context) error {
	return nil
}

func (d *BaiduNetdisk) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	files, err := d.getFiles(dir.GetPath())
	if err != nil {
		return nil, err
	}
	return utils.SliceConvert(files, func(src File) (model.Obj, error) {
		return fileToObj(src), nil
	})
}

func (d *BaiduNetdisk) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	if d.DownloadAPI == "crack" {
		return d.linkCrack(file, args)
	}
	return d.linkOfficial(file, args)
}

func (d *BaiduNetdisk) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) (model.Obj, error) {
	var newDir File
	_, err := d.create(stdpath.Join(parentDir.GetPath(), dirName), 0, 1, "", "", &newDir)
	if err != nil {
		return nil, err
	}
	return fileToObj(newDir), nil
}

func (d *BaiduNetdisk) Move(ctx context.Context, srcObj, dstDir model.Obj) (model.Obj, error) {
	data := []base.Json{
		{
			"path":    srcObj.GetPath(),
			"dest":    dstDir.GetPath(),
			"newname": srcObj.GetName(),
		},
	}
	_, err := d.manage("move", data)
	if err != nil {
		return nil, err
	}
	if srcObj, ok := srcObj.(*model.ObjThumb); ok {
		srcObj.SetPath(stdpath.Join(dstDir.GetPath(), srcObj.GetName()))
		srcObj.Modified = time.Now()
		return srcObj, nil
	}
	return nil, nil
}

func (d *BaiduNetdisk) Rename(ctx context.Context, srcObj model.Obj, newName string) (model.Obj, error) {
	data := []base.Json{
		{
			"path":    srcObj.GetPath(),
			"newname": newName,
		},
	}
	_, err := d.manage("rename", data)
	if err != nil {
		return nil, err
	}

	if srcObj, ok := srcObj.(*model.ObjThumb); ok {
		srcObj.SetPath(stdpath.Join(stdpath.Dir(srcObj.GetPath()), newName))
		srcObj.Name = newName
		srcObj.Modified = time.Now()
		return srcObj, nil
	}
	return nil, nil
}

func (d *BaiduNetdisk) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	data := []base.Json{
		{
			"path":    srcObj.GetPath(),
			"dest":    dstDir.GetPath(),
			"newname": srcObj.GetName(),
		},
	}
	_, err := d.manage("copy", data)
	return err
}

func (d *BaiduNetdisk) Remove(ctx context.Context, obj model.Obj) error {
	data := []string{obj.GetPath()}
	_, err := d.manage("delete", data)
	return err
}

func (d *BaiduNetdisk) Put(ctx context.Context, dstDir model.Obj, stream model.FileStreamer, up driver.UpdateProgress) (model.Obj, error) {
	tempFile, err := utils.CreateTempFile(stream.GetReadCloser(), stream.GetSize())
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = tempFile.Close()
		_ = os.Remove(tempFile.Name())
	}()

	streamSize := stream.GetSize()
	count := int(math.Max(math.Ceil(float64(streamSize)/float64(DefaultSliceSize)), 1))
	lastBlockSize := streamSize % DefaultSliceSize
	if streamSize > 0 && lastBlockSize == 0 {
		lastBlockSize = DefaultSliceSize
	}

	//cal md5 for first 256k data
	const SliceSize int64 = 256 * 1024
	// cal md5
	blockList := make([]string, 0, count)
	byteSize := DefaultSliceSize
	fileMd5H := md5.New()
	sliceMd5H := md5.New()
	sliceMd5H2 := md5.New()
	slicemd5H2Write := utils.LimitWriter(sliceMd5H2, SliceSize)

	for i := 1; i <= count; i++ {
		if utils.IsCanceled(ctx) {
			return nil, ctx.Err()
		}
		if i == count {
			byteSize = lastBlockSize
		}
		_, err := io.CopyN(io.MultiWriter(fileMd5H, sliceMd5H, slicemd5H2Write), tempFile, byteSize)
		if err != nil && err != io.EOF {
			return nil, err
		}
		blockList = append(blockList, hex.EncodeToString(sliceMd5H.Sum(nil)))
		sliceMd5H.Reset()
	}
	contentMd5 := hex.EncodeToString(fileMd5H.Sum(nil))
	sliceMd5 := hex.EncodeToString(sliceMd5H2.Sum(nil))
	blockListStr, _ := utils.Json.MarshalToString(blockList)

	// step.1 预上传
	rawPath := stdpath.Join(dstDir.GetPath(), stream.GetName())
	path := encodeURIComponent(rawPath)

	data := fmt.Sprintf("path=%s&size=%d&isdir=0&autoinit=1&rtype=3&block_list=%s&content-md5=%s&slice-md5=%s",
		path, streamSize,
		blockListStr,
		contentMd5, sliceMd5)
	params := map[string]string{
		"method": "precreate",
	}
	log.Debugf("[baidu_netdisk] precreate data: %s", data)
	var precreateResp PrecreateResp
	_, err = d.post("/xpan/file", params, data, &precreateResp)
	if err != nil {
		return nil, err
	}
	log.Debugf("%+v", precreateResp)
	if precreateResp.ReturnType == 2 {
		//rapid upload, since got md5 match from baidu server
		if err != nil {
			return nil, err
		}
		return fileToObj(precreateResp.File), nil
	}

	// step.2 上传分片
	threadG, upCtx := errgroup.NewGroupWithContext(ctx, d.uploadThread,
		retry.Attempts(3),
		retry.Delay(time.Second),
		retry.DelayType(retry.BackOffDelay))
	for _, partseq := range precreateResp.BlockList {
		if utils.IsCanceled(upCtx) {
			break
		}

		partseq, offset, byteSize := partseq, int64(partseq)*DefaultSliceSize, DefaultSliceSize
		if partseq+1 == count {
			byteSize = lastBlockSize
		}
		threadG.Go(func(ctx context.Context) error {
			params := map[string]string{
				"method":       "upload",
				"access_token": d.AccessToken,
				"type":         "tmpfile",
				"path":         path,
				"uploadid":     precreateResp.Uploadid,
				"partseq":      strconv.Itoa(partseq),
			}
			err := d.uploadSlice(ctx, params, stream.GetName(), io.NewSectionReader(tempFile, offset, byteSize))
			if err != nil {
				return err
			}
			up(int(threadG.Success()) * 100 / len(precreateResp.BlockList))
			return nil
		})
	}
	if err = threadG.Wait(); err != nil {
		return nil, err
	}

	// step.3 创建文件
	var newFile File
	_, err = d.create(rawPath, streamSize, 0, precreateResp.Uploadid, blockListStr, &newFile)
	if err != nil {
		return nil, err
	}
	return fileToObj(newFile), nil
}
func (d *BaiduNetdisk) uploadSlice(ctx context.Context, params map[string]string, fileName string, file io.Reader) error {
	res, err := base.RestyClient.R().
		SetContext(ctx).
		SetQueryParams(params).
		SetFileReader("file", fileName, file).
		Post(BaiduFileAPI)
	if err != nil {
		return err
	}
	log.Debugln(res.RawResponse.Status + res.String())
	errCode := utils.Json.Get(res.Body(), "error_code").ToInt()
	errNo := utils.Json.Get(res.Body(), "errno").ToInt()
	if errCode != 0 || errNo != 0 {
		return errs.NewErr(errs.StreamIncomplete, "error in uploading to baidu, will retry. response=%s", res.String())
	}
	return nil
}

var _ driver.Driver = (*BaiduNetdisk)(nil)
