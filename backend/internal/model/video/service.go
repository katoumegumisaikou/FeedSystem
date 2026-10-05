package video

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"feed-system/internal/pkg/errs"
	"feed-system/internal/pkg/sfcache"
	"feed-system/internal/util/filetype"
)

// MaxVideoSize 单个视频文件上限:1 GiB(1024^3 字节)。
const MaxVideoSize int64 = 1 << 30

// VideoChunkSize 每个分片大小:5 MiB(5×1024^2 字节)。
const VideoChunkSize int64 = 5 << 20

const uploadDeclarationTTL = 24 * time.Hour

// 上传会话状态
const (
	uploadStatusPending   int8 = 0 // 分片还没收齐
	uploadStatusCompleted int8 = 1 // 已合并完成,结果看 UploadDeclaration.VideoID
)

// completedDeclarationTTL 合并完成后会话再保留一段时间。
// 不能合并完就删:客户端可能没收到响应而重试(响应丢包),报「会话不存在」会让用户以为 1GB 白传了
const completedDeclarationTTL = time.Hour

// mergeLockTTL 合并锁的存活时间。进程崩在合并中途时,锁靠它自动过期
const mergeLockTTL = 2 * time.Minute

// UploadDeclaration 记录一次分片上传的文件声明,用于保存到 Redis。
type UploadDeclaration struct {
	UploadID    string    `json:"upload_id"`
	UserID      int64     `json:"user_id"`
	Filename    string    `json:"filename"`
	FileSize    int64     `json:"file_size"`  // 单位 Byte
	FileHash    string    `json:"file_hash"`  // SHA-256
	ChunkSize   int64     `json:"chunk_size"` // 单位 Byte
	TotalChunks int       `json:"total_chunks"`
	CreatedAt   time.Time `json:"created_at"`

	Status  int8  `json:"status"`   // 会话状态,见 uploadStatus*
	VideoID int64 `json:"video_id"` // 合并完成后建的草稿视频 ID
}

// UserInfoProvider 视频模块需要的用户信息。在 video 包定义接口、由 main.go 用 account 的实现
// 适配进来(依赖倒置):直接 import account 会让两个模块双向耦合,以后 account 想引 video 就成环了
type UserInfoProvider interface {
	// GetAuthorInfo 取作者的用户名和头像,写入 videos 的冗余字段
	GetAuthorInfo(ctx context.Context, userID int64) (username, avatarURL string, err error)
}

// VideoService 视频业务层
type VideoService struct {
	videorepo VideoRepository
	rdb       *redis.Client    // 用 Redis Bitmap 记录已上传分片
	users     UserInfoProvider // 取作者信息,可为 nil
}

// NewVideoService 构造 VideoService。
// users 可为 nil(作者信息留空);rdb 为 nil 时上传接口报「上传服务未启用」
func NewVideoService(videorepo VideoRepository, rdb *redis.Client, users UserInfoProvider) *VideoService {
	return &VideoService{videorepo: videorepo, rdb: rdb, users: users}
}

func uploadBitmapKey(uploadID string) string {
	return "feed:upload:bitmap:" + uploadID
}

func uploadDeclarationKey(uploadID string) string {
	return "feed:upload:declaration:" + uploadID
}

// mergeLockKey 合并锁。并发调 complete 时用它保证只有一个请求真正合并
func mergeLockKey(uploadID string) string {
	return "feed:upload:merging:" + uploadID
}

// markChunkUploaded 标记分片已上传(Redis Bitmap 每 bit 对应一个分片)。
func (s *VideoService) markChunkUploaded(ctx context.Context, uploadID string, chunkIndex int64) error {
	if s.rdb == nil {
		return errs.ErrInternal.WithMsg("上传服务未启用")
	}
	if chunkIndex < 0 {
		return errs.ErrInvalidParam.WithMsg("分片序号无效")
	}
	key := uploadBitmapKey(uploadID)
	if err := s.rdb.SetBit(ctx, key, chunkIndex, 1).Err(); err != nil {
		return errs.ErrInternal.WithMsg("记录分片状态失败")
	}
	if err := s.rdb.Expire(ctx, key, uploadDeclarationTTL).Err(); err != nil {
		return errs.ErrInternal.WithMsg("设置分片状态有效期失败")
	}
	return nil
}

func (s *VideoService) isChunkUploaded(ctx context.Context, uploadID string, chunkIndex int64) (bool, error) {
	if s.rdb == nil {
		return false, errs.ErrInternal.WithMsg("上传服务未启用")
	}
	if chunkIndex < 0 {
		return false, errs.ErrInvalidParam.WithMsg("分片序号无效")
	}
	bit, err := s.rdb.GetBit(ctx, uploadBitmapKey(uploadID), chunkIndex).Result()
	return bit == 1, err
}

func (s *VideoService) InitChunkUpload(ctx context.Context, req InitChunkUploadRequest, userID int64) (*InitChunkUploadResp, error) {
	if s.rdb == nil {
		return nil, errs.ErrInternal.WithMsg("上传服务未启用")
	}
	if userID <= 0 {
		return nil, errs.ErrUnauthorized.WithMsg("用户未登录")
	}
	if req.FileSize <= 0 {
		return nil, errs.ErrInvalidParam.WithMsg("视频文件大小必须大于 0")
	}
	if req.FileSize > MaxVideoSize {
		return nil, errs.ErrInvalidParam.WithMsg("视频文件大小不能超过 1 GiB")
	}

	chunkCounts := (req.FileSize + VideoChunkSize - 1) / VideoChunkSize
	uploadID := uuid.NewString()
	declaration := UploadDeclaration{
		UploadID:    uploadID,
		UserID:      userID,
		Filename:    req.Filename,
		FileSize:    req.FileSize,
		FileHash:    req.FileHash,
		ChunkSize:   VideoChunkSize,
		TotalChunks: int(chunkCounts),
		CreatedAt:   time.Now(),
	}
	data, err := json.Marshal(declaration)
	if err != nil {
		return nil, errs.ErrInternal.WithMsg("创建上传声明失败")
	}
	if err := s.rdb.Set(ctx, uploadDeclarationKey(uploadID), data, uploadDeclarationTTL).Err(); err != nil {
		return nil, errs.ErrInternal.WithMsg("保存上传声明失败")
	}

	return &InitChunkUploadResp{
		UploadID:    uploadID,
		ChunkSize:   VideoChunkSize,
		TotalChunks: int(chunkCounts),
	}, nil
}

// 存储路径
//
// 都是相对路径,基准是进程工作目录 —— Makefile 的后端 target 全部 cd backend 后启动,
// 所以运行时产物统一落在 backend/storage/ 之下。换机器 / 进容器不用改代码

// VideoStorageDir 最终视频存储根目录
const VideoStorageDir = "storage/videos"

// VideoURLPrefix 视频对外 URL 前缀,由 main.go 静态路由挂载;必须与 VideoStorageDir 对应,否则入库 URL 会 404
const VideoURLPrefix = "/videos"

// CoverURLPrefix 封面对外 URL 前缀。
// cover_url 只接受这个前缀开头的站内路径,校验见 validCoverURL
const CoverURLPrefix = "/covers"

// CoverStorageDir 封面存储根目录,与 CoverURLPrefix 一一对应
const CoverStorageDir = "storage/covers"

// chunkStorageDir 分片上传的临时根目录,每个会话一个子目录。
// 不能借用系统 /tmp:多数机器的 /tmp 是 tmpfs(内存盘),大视频会吃光内存,
// 而且跨文件系统 rename 会返回 EXDEV
//
// TODO(待议④): 这个目录下的孤儿目录目前没有任何东西回收,来源有四种 ——
// 客户端传一半放弃、重试的分片在 RemoveAll 之后才落盘、合并中途失败、进程崩溃。
// 计划另起一个独立进程定期清理(不放 web 进程里:多实例部署会重复扫):
// 扫一级子目录,目录名是 uuid 的,查 feed:upload:declaration:<目录名> 是否还存在,
// 不存在就 RemoveAll —— 这个判据零误杀,declaration 还在说明用户可能还在传。
// 再加一条「mtime 超过 7 天就删」兜底。
const chunkStorageDir = "storage/tmp/video_chunk"

func sessionChunkDir(uploadID string) string {
	return filepath.Join(chunkStorageDir, uploadID)
}

// chunkPathAt 第 index 个分片的路径。分片名就是序号,不含任何客户端输入
func chunkPathAt(uploadID string, index int) string {
	return filepath.Join(sessionChunkDir(uploadID), strconv.Itoa(index))
}

// chunkSizeAt 第 index 片的期望字节数:最后一片通常不足 ChunkSize
func chunkSizeAt(decl *UploadDeclaration, index int) int64 {
	if index == decl.TotalChunks-1 {
		if rest := decl.FileSize % decl.ChunkSize; rest != 0 {
			return rest
		}
	}
	return decl.ChunkSize
}

// writeChunkFile 把一个分片写进会话目录。先写随机名临时文件再 rename 成序号:
// 同文件系统 rename 原子,重复上传即整体覆盖,不会留下「写了一半却被当成有效分片」的文件。
// 临时文件必须建在目标目录里 —— os.CreateTemp("", ...) 落系统临时目录,跨文件系统 rename 返回 EXDEV
func writeChunkFile(dir string, index int, fh *multipart.FileHeader) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errs.ErrInternal.WithMsg("创建分片目录失败")
	}

	src, err := fh.Open()
	if err != nil {
		return errs.ErrInternal.WithMsg("读取分片内容失败")
	}
	defer src.Close()

	tmp, err := os.CreateTemp(dir, ".part-*")
	if err != nil {
		return errs.ErrInternal.WithMsg("创建分片临时文件失败")
	}
	tmpName := tmp.Name()

	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return errs.ErrInternal.WithMsg("写入分片失败")
	}
	// Close 可能携带延迟写入错误,必须在 rename 之前判,否则会把没落稳的文件当成正式分片
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return errs.ErrInternal.WithMsg("写入分片失败")
	}

	if err := os.Rename(tmpName, filepath.Join(dir, strconv.Itoa(index))); err != nil {
		_ = os.Remove(tmpName)
		return errs.ErrInternal.WithMsg("保存分片失败")
	}
	return nil
}

// loadDeclaration 从 Redis 取上传声明。会话不存在时会顺带清理残留的分片目录
func (s *VideoService) loadDeclaration(ctx context.Context, uploadID string) (*UploadDeclaration, error) {
	if s.rdb == nil {
		return nil, errs.ErrInternal.WithMsg("上传服务未启用")
	}

	// uploadID 会被当目录名拼进文件路径,且来自客户端。不先卡格式的话,
	// upload_id = "../.." 之类的值就能让后面的 MkdirAll / RemoveAll 作用到任意路径
	if _, err := uuid.Parse(uploadID); err != nil {
		return nil, errs.ErrInvalidParam.WithMsg("上传会话 ID 格式不正确")
	}

	raw, err := s.rdb.Get(ctx, uploadDeclarationKey(uploadID)).Result()
	if errors.Is(err, redis.Nil) {
		_ = os.RemoveAll(sessionChunkDir(uploadID)) // 会话没了,分片也没用了
		return nil, errs.ErrNotFound.WithMsg("上传会话不存在或已过期")
	}
	if err != nil {
		return nil, errs.ErrInternal.WithMsg("读取上传会话失败")
	}

	var decl UploadDeclaration
	if err := json.Unmarshal([]byte(raw), &decl); err != nil {
		return nil, errs.ErrInternal.WithMsg("上传会话数据损坏")
	}
	return &decl, nil
}

// UploadChunk 接收一个分片并落盘,返回当前上传进度
func (s *VideoService) UploadChunk(ctx context.Context, req UploadChunkRequest, userID int64, fileHeader *multipart.FileHeader) (*UploadChunkResp, error) {
	if userID <= 0 {
		return nil, errs.ErrUnauthorized.WithMsg("用户未登录")
	}

	// 1. 取上传声明(内含 uploadID 格式校验)
	decl, err := s.loadDeclaration(ctx, req.UploadID)
	if err != nil {
		return nil, err
	}

	// 2. 校验归属与会话状态。归属:强鉴权只证明「已登录」,不证明「这个会话是你的」。
	// 状态:已完成的会话再收分片会重建已清理的目录和 bitmap,还给客户端一个误导的「上传成功」。
	// 合并中那段空窗挡不住(那时 Status 还是 pending),代价只是孤儿目录,由定期清理兜底
	if decl.UserID != userID {
		return nil, errs.ErrForbidden.WithMsg("无权操作该上传会话")
	}
	if decl.Status == uploadStatusCompleted {
		return nil, errs.ErrConflict.WithMsg("该上传已完成")
	}

	// 3. 分片序号必须落在 [0, TotalChunks),否则会写出 TotalChunks 之外的野文件
	if req.ChunkIndex < 0 || req.ChunkIndex >= decl.TotalChunks {
		return nil, errs.ErrInvalidParam.WithMsg("分片序号超出范围")
	}

	// 4. 大小必须与声明吻合,否则拼出的文件长度对不上,而发现时用户已白传整个文件
	if want := chunkSizeAt(decl, req.ChunkIndex); fileHeader.Size != want {
		return nil, errs.ErrInvalidParam.WithMsg(
			fmt.Sprintf("分片大小不符:第 %d 片应为 %d 字节", req.ChunkIndex, want))
	}

	if err := writeChunkFile(sessionChunkDir(decl.UploadID), req.ChunkIndex, fileHeader); err != nil {
		return nil, err
	}

	// 6. 标记已上传,必须排在落盘之后 —— 反过来崩在两步之间会留下
	//    「bitmap 说有、文件其实不在」的静默缺片,且永不重传
	if err := s.markChunkUploaded(ctx, decl.UploadID, int64(req.ChunkIndex)); err != nil {
		return nil, err
	}

	// 7. 汇总进度。重复上传的分片不会让计数偏大 —— bitmap 是按位去重的
	uploaded, err := s.rdb.BitCount(ctx, uploadBitmapKey(decl.UploadID), nil).Result()
	if err != nil {
		return nil, errs.ErrInternal.WithMsg("读取上传进度失败")
	}
	count := int(uploaded)

	return &UploadChunkResp{
		UploadID:      decl.UploadID,
		ChunkIndex:    req.ChunkIndex,
		Uploaded:      true,
		Completed:     count == decl.TotalChunks,
		UploadedCount: count,
		TotalChunks:   decl.TotalChunks,
	}, nil
}

// 合并

func videoDir(userID int64) string {
	return filepath.Join(VideoStorageDir, strconv.FormatInt(userID, 10))
}

type mergeResult struct {
	Path string // 临时文件路径
	Size int64  // 实际字节数
	Hash string // 实际 sha256(hex)
}

// readChunkHead 读第一片前 HeaderSize 字节判容器类型;第一片就是文件开头,不用等整个文件拼完
func readChunkHead(uploadID string) ([]byte, error) {
	f, err := os.Open(chunkPathAt(uploadID, 0))
	if err != nil {
		return nil, errs.ErrInternal.WithMsg("分片缺失,无法合并")
	}
	defer f.Close()

	head := make([]byte, filetype.HeaderSize)
	n, err := io.ReadFull(f, head)
	// 文件比 HeaderSize 还短是合法的(极小视频),n 就是实际长度
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, errs.ErrInternal.WithMsg("读取分片头部失败")
	}
	return head[:n], nil
}

// concatChunks 按序号拼接,边拼边算 sha256(1GB 文件省一次完整读盘);先写随机名临时文件,判出容器类型后再 rename 成最终名
func concatChunks(decl *UploadDeclaration) (*mergeResult, error) {
	dir := videoDir(decl.UserID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, errs.ErrInternal.WithMsg("创建视频目录失败")
	}

	tmp, err := os.CreateTemp(dir, ".merge-*")
	if err != nil {
		return nil, errs.ErrInternal.WithMsg("创建视频临时文件失败")
	}
	tmpPath := tmp.Name()

	// 统一失败出口:清掉半成品,不留残缺文件
	fail := func(msg string) (*mergeResult, error) {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return nil, errs.ErrInternal.WithMsg(msg)
	}

	hasher := sha256.New()
	w := io.MultiWriter(tmp, hasher)

	var size int64
	for i := 0; i < decl.TotalChunks; i++ {
		src, err := os.Open(chunkPathAt(decl.UploadID, i))
		if err != nil {
			return fail("分片缺失,无法合并")
		}
		n, err := io.Copy(w, src)
		_ = src.Close()
		if err != nil {
			return fail("合并分片失败")
		}
		size += n
	}

	// close 的错误必须在 rename 之前判,否则会把没落稳的文件当成正式视频
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return nil, errs.ErrInternal.WithMsg("写入视频文件失败")
	}

	return &mergeResult{Path: tmpPath, Size: size, Hash: hex.EncodeToString(hasher.Sum(nil))}, nil
}

// CompleteChunkUpload 合并分片、校验完整性,并把结果登记成一条草稿视频
func (s *VideoService) CompleteChunkUpload(ctx context.Context, req CompleteChunkUploadReq, userID int64) (*CompleteChunkUploadResp, error) {
	if userID <= 0 {
		return nil, errs.ErrUnauthorized.WithMsg("用户未登录")
	}

	decl, err := s.loadDeclaration(ctx, req.UploadID)
	if err != nil {
		return nil, err
	}
	if decl.UserID != userID {
		return nil, errs.ErrForbidden.WithMsg("无权操作该上传会话")
	}

	// 已经合并过了:客户端重试(响应丢包)走这里,原样返回,不能报「会话不存在」
	if decl.Status == uploadStatusCompleted {
		return &CompleteChunkUploadResp{VideoID: decl.VideoID, FileSize: decl.FileSize}, nil
	}

	uploaded, err := s.rdb.BitCount(ctx, uploadBitmapKey(decl.UploadID), nil).Result()
	if err != nil {
		return nil, errs.ErrInternal.WithMsg("读取上传进度失败")
	}
	if int(uploaded) != decl.TotalChunks {
		return nil, errs.ErrInvalidParam.WithMsg(
			fmt.Sprintf("还有 %d 个分片没上传", decl.TotalChunks-int(uploaded)))
	}

	// 2. 抢合并锁。并发调 complete 时只有一个真正合并,否则会同时往同一路径拼文件
	locked, err := s.rdb.SetNX(ctx, mergeLockKey(decl.UploadID), 1, mergeLockTTL).Result()
	if err != nil {
		return nil, errs.ErrInternal.WithMsg("获取合并锁失败")
	}
	if !locked {
		return nil, errs.ErrTooFrequent.WithMsg("文件正在合并中,请稍后重试")
	}
	// 失败时释放,让客户端能重试;成功时会话已标记完成,删掉也无妨
	defer func() { _ = s.rdb.Del(ctx, mergeLockKey(decl.UploadID)).Err() }()

	// 3. 判容器类型。后缀由魔数推导,不用客户端可控的 decl.Filename —— 传 evil.html 会被存成 .html,静态伺服时造成 XSS
	head, err := readChunkHead(decl.UploadID)
	if err != nil {
		return nil, err
	}
	mime, ok := filetype.DetectVideo(head)
	if !ok {
		return nil, errs.ErrInvalidParam.WithMsg("文件不是受支持的视频格式(mp4 / webm / avi)")
	}
	ext, ok := filetype.ExtForMIME(mime)
	if !ok {
		return nil, errs.ErrInvalidParam.WithMsg("文件不是受支持的视频格式(mp4 / webm / avi)")
	}

	merged, err := concatChunks(decl)
	if err != nil {
		return nil, err
	}

	// 5. 完整性校验 —— init 时收的 file_hash 就是为了这一步
	if merged.Hash != decl.FileHash {
		_ = os.Remove(merged.Path)
		return nil, errs.ErrInvalidParam.WithMsg("文件校验失败,请重新上传")
	}
	if merged.Size != decl.FileSize {
		_ = os.Remove(merged.Path)
		return nil, errs.ErrInvalidParam.WithMsg("文件大小与声明不符,请重新上传")
	}

	// 6. 定最终名并原子改名(临时文件和目标同目录,同文件系统)。
	// 时间前缀让文件名排序≈上传时间排序,uuid 保证同一秒内多次上传也不撞名
	finalName := time.Now().Format("20060102150405") + "-" + uuid.NewString() + ext
	finalPath := filepath.Join(videoDir(decl.UserID), finalName)
	if err := os.Rename(merged.Path, finalPath); err != nil {
		_ = os.Remove(merged.Path)
		return nil, errs.ErrInternal.WithMsg("保存视频失败")
	}

	// 文件已经落盘,后面写数据库任何一步失败都要删掉它,避免留下孤儿文件
	rollback := func() { _ = os.Remove(finalPath) }

	username, avatarURL := "", ""
	if s.users != nil {
		username, avatarURL, err = s.users.GetAuthorInfo(ctx, userID)
		if err != nil {
			rollback()
			return nil, errs.ErrInternal.WithMsg("读取作者信息失败")
		}
	}

	video := &Video{
		AuthorID:  userID,
		Username:  username,
		AvatarURL: avatarURL,
		Title:     decl.Filename, // 标题先用原始文件名,发布时再改
		PlayURL:   VideoURLPrefix + "/" + strconv.FormatInt(userID, 10) + "/" + finalName,
		CoverURL:  "",          // 封面是另一个文件,走编辑接口补
		Status:    StatusDraft, // 草稿:还没编辑标题和封面
	}
	if err := s.videorepo.CreateVideo(ctx, video); err != nil {
		rollback()
		return nil, errs.ErrInternal.WithMsg("创建视频记录失败")
	}

	// 7. 收尾:分片目录不再需要;会话标记完成并缩短 TTL,让重试能拿到同一结果
	_ = os.RemoveAll(sessionChunkDir(decl.UploadID))
	_ = s.rdb.Del(ctx, uploadBitmapKey(decl.UploadID)).Err()

	decl.Status = uploadStatusCompleted
	decl.VideoID = video.ID
	if data, mErr := json.Marshal(decl); mErr == nil {
		s.rdb.Set(ctx, uploadDeclarationKey(decl.UploadID), data, completedDeclarationTTL)
	}

	return &CompleteChunkUploadResp{VideoID: video.ID, FileSize: merged.Size}, nil
}

// 编辑与发布

// validCoverURL 校验封面地址是站内路径。
// 这个值前端会直接拿去当 img src,放任客户端传就等于允许存 javascript:
// 或外站地址;顺手挡掉 "..",免得拼出目录穿越
func validCoverURL(u string) bool {
	return strings.HasPrefix(u, CoverURLPrefix+"/") && !strings.Contains(u, "..")
}

// findVideo 取视频,不存在返回 404,仓储出错记日志并返回 500
func (s *VideoService) findVideo(ctx context.Context, videoID int64) (*Video, error) {
	if videoID <= 0 {
		return nil, errs.ErrInvalidParam.WithMsg("视频 ID 无效")
	}

	video, err := s.videorepo.FindVideoByID(ctx, videoID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, errs.ErrNotFound.WithMsg("视频不存在")
		}
		slog.ErrorContext(ctx, "查询视频失败", "video_id", videoID, "err", err)
		return nil, errs.ErrInternal.WithMsg("查询视频失败")
	}
	return video, nil
}

// loadOwnedVideo 取视频并校验归属。
// 不是作者返回 403 而不是 404:项目其他接口(UploadChunk)也是这个口径,不靠 404 掩盖存在性
func (s *VideoService) loadOwnedVideo(ctx context.Context, videoID, userID int64) (*Video, error) {
	if userID <= 0 {
		return nil, errs.ErrUnauthorized.WithMsg("用户未登录")
	}

	video, err := s.findVideo(ctx, videoID)
	if err != nil {
		return nil, err
	}
	if video.AuthorID != userID {
		return nil, errs.ErrForbidden.WithMsg("无权操作该视频")
	}
	return video, nil
}

// GetUserLikeStatuses 批量查询用户对给定视频的点赞状态;结果 map 中会为每个 ID 返回 true 或 false。
func (s *VideoService) GetUserLikeStatuses(ctx context.Context, userID int64, videoIDs []int64) (map[int64]bool, error) {
	if userID <= 0 {
		return nil, errs.ErrUnauthorized.WithMsg("用户未登录")
	}

	statuses := make(map[int64]bool, len(videoIDs))
	uniqueVideoIDs := make([]int64, 0, len(videoIDs))
	for _, videoID := range videoIDs {
		if videoID <= 0 {
			return nil, errs.ErrInvalidParam.WithMsg("视频 ID 无效")
		}
		if _, exists := statuses[videoID]; exists {
			continue
		}
		statuses[videoID] = false
		uniqueVideoIDs = append(uniqueVideoIDs, videoID)
	}
	if len(uniqueVideoIDs) == 0 {
		return statuses, nil
	}

	likedVideoIDs, err := s.videorepo.ListLikedVideoIDs(ctx, userID, uniqueVideoIDs)
	if err != nil {
		slog.ErrorContext(ctx, "批量查询视频点赞状态失败", "user_id", userID, "video_count", len(uniqueVideoIDs), "err", err)
		return nil, errs.ErrInternal
	}
	for _, videoID := range likedVideoIDs {
		if _, requested := statuses[videoID]; requested {
			statuses[videoID] = true
		}
	}
	return statuses, nil
}

func (s *VideoService) setLikeStatuses(ctx context.Context, userID int64, items []*VideoResp) error {
	if userID <= 0 || len(items) == 0 {
		return nil
	}

	videoIDs := make([]int64, 0, len(items))
	for _, item := range items {
		if item != nil {
			videoIDs = append(videoIDs, item.ID)
		}
	}
	statuses, err := s.GetUserLikeStatuses(ctx, userID, videoIDs)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item == nil {
			continue
		}
		liked := statuses[item.ID]
		item.IsLike = &liked
	}
	return nil
}

func (s *VideoService) videoRespForUser(ctx context.Context, userID int64, item *Video) (*VideoResp, error) {
	resp := toVideoResp(item)
	if err := s.setLikeStatuses(ctx, userID, []*VideoResp{resp}); err != nil {
		return nil, err
	}
	return resp, nil
}

// videoDetailCacheTTL 详情缓存的存活时间。取 5 分钟与文件路由的可见性判定同档 ——
// 两者缓存的都是同一份视频状态的投影,TTL 不一致只会凭空多出一个
// 「详情页能看、文件却 404」的窗口。
//
// 代价:play_count 只由 TTL 兜底。播放是最热的写路径,每次上报都删缓存等于缓存失效,
// 所以上报不失效 —— 详情页的计数最多偏小一个 TTL
const videoDetailCacheTTL = 5 * time.Minute

// videoDetailKey 详情缓存 key。前缀必须全进程唯一:sfcache 的去重域是进程级的,
// 撞 key 会让一方拿到另一方的结果
func videoDetailKey(videoID int64) string {
	return "video:detail:" + strconv.FormatInt(videoID, 10)
}

// invalidateVideoDetail 清掉详情缓存。
//
// 删失败只记日志:缓存是加速层,删不掉最多让旧值多活一个 TTL,
// 不该让一次已经落库成功的编辑反过来报错
func (s *VideoService) invalidateVideoDetail(ctx context.Context, videoID int64) {
	if s.rdb == nil {
		return
	}
	if err := s.rdb.Del(ctx, videoDetailKey(videoID)).Err(); err != nil {
		slog.ErrorContext(ctx, "删除视频详情缓存失败", "video_id", videoID, "err", err)
	}
}

// GetVideoDetail 取视频详情(GET /videos/:id)。
//
// requesterID 为 0 表示匿名(软鉴权没拿到 token)。
// 未发布的一律 404,作者本人除外 —— 用 404 而不是 403:403 等于告诉遍历者
// 「这个 ID 存在」,草稿的 ID 边界就被探出来了
//
// 缓存的是 Video 实体,不是响应:VideoResp.IsLike 是 per-user 的,
// 整份响应缓存下来会把上一个用户的点赞状态发给下一个用户。
// 缓存只在 get 这一层 —— findVideo 同时喂着 UpdateVideo / PublishVideo 的状态守卫,
// 那些守卫必须读精确值,陈旧状态会让「已下架不能编辑」这类规则失效
func (s *VideoService) GetVideoDetail(ctx context.Context, videoID, requesterID int64) (*VideoResp, error) {
	video, err := sfcache.Load(ctx, s.rdb, videoDetailKey(videoID), videoDetailCacheTTL,
		func(loadCtx context.Context) (*Video, error) {
			return s.findVideo(loadCtx, videoID)
		})
	if err != nil {
		return nil, err
	}

	// 可见性判定必须在缓存之外:同一份实体,作者看得到、别人看不到。
	// 404 也不缓存 —— 详情页不像播放器的 Range 请求那样反复打同一个死链
	if video.Status != StatusPublished && video.AuthorID != requesterID {
		return nil, errs.ErrNotFound.WithMsg("视频不存在")
	}
	return s.videoRespForUser(ctx, requesterID, video)
}

// UpdateVideo 编辑视频元数据(PUT /videos/:id)
func (s *VideoService) UpdateVideo(ctx context.Context, videoID, userID int64, req UpdateVideoReq) (*VideoResp, error) {
	video, err := s.loadOwnedVideo(ctx, videoID, userID)
	if err != nil {
		return nil, err
	}
	// 下架是运营决定,不该被作者改回去
	if video.Status == StatusRemoved {
		return nil, errs.ErrConflict.WithMsg("视频已下架,不能编辑")
	}
	if req.CoverURL != "" && !validCoverURL(req.CoverURL) {
		return nil, errs.ErrInvalidParam.WithMsg("封面地址必须是站内路径")
	}

	fields := map[string]any{
		"title":       req.Title,
		"description": req.Description,
	}
	if req.CoverURL != "" {
		fields["cover_url"] = req.CoverURL
	}
	// updated_at 不用写:GORM 对 map 更新也会自动填(AutoUpdateTime 分支)
	if err := s.videorepo.UpdateVideoFields(ctx, videoID, fields); err != nil {
		slog.ErrorContext(ctx, "更新视频失败", "video_id", videoID, "user_id", userID, "err", err)
		return nil, errs.ErrInternal.WithMsg("更新视频失败")
	}
	// 写成功之后才删:删早了,并发的读会把改动前的旧值重新灌回缓存
	s.invalidateVideoDetail(ctx, videoID)

	video.Title = req.Title
	video.Description = req.Description
	if req.CoverURL != "" {
		video.CoverURL = req.CoverURL
	}
	return s.videoRespForUser(ctx, userID, video)
}

// PublishVideo 把草稿翻成已发布(POST /videos/:id/publish)
func (s *VideoService) PublishVideo(ctx context.Context, videoID, userID int64) (*VideoResp, error) {
	video, err := s.loadOwnedVideo(ctx, videoID, userID)
	if err != nil {
		return nil, err
	}

	switch video.Status {
	case StatusPublished:
		// 重复发布当成功:客户端响应丢包重试会走到这,
		// 报错会让用户以为没发出去
		return s.videoRespForUser(ctx, userID, video)
	case StatusDraft:
		// 继续往下
	default:
		return nil, errs.ErrConflict.WithMsg("当前状态不能发布")
	}

	// 发布时刻由 MarkPublished 内部取 time.Now(),不能复用 video.CreatedAt:
	// created_at 是上传时刻,一个传完搁置很久才发布的草稿会带着旧时刻入队,
	// 被 fan-out 的 7 天窗口当场裁掉,视频就永远进不了任何粉丝的关注流
	if err := s.videorepo.MarkPublished(ctx, videoID, video.AuthorID); err != nil {
		slog.ErrorContext(ctx, "发布视频失败", "video_id", videoID, "user_id", userID, "err", err)
		return nil, errs.ErrInternal.WithMsg("发布视频失败")
	}
	// 状态变了,详情缓存里的旧状态会让匿名用户继续 404
	s.invalidateVideoDetail(ctx, videoID)
	video.Status = StatusPublished
	return s.videoRespForUser(ctx, userID, video)
}

// visibleTo 视频对 requesterID 是否可见,requesterID 为 0 表示游客(软鉴权没拿到 token)。
//
// 已下架对所有人不可见 —— 含作者本人;草稿 / 转码中 / 转码失败只对作者可见。
// 文件路由(video.PlayAccess)与上报接口共用这一处口径,改判定只改这里。
//
// 注意 GetVideoDetail 仍是另一套口径(作者能看到自己已下架视频的详情):
// 统一它会改变既有接口的行为,所以先不动,两边的不一致记在这里
func visibleTo(status int8, authorID, requesterID int64) bool {
	if status == StatusRemoved {
		return false
	}
	return status == StatusPublished || (requesterID > 0 && authorID == requesterID)
}

// ReportPlay 记录一次播放(POST /videos/:id/play)。
//
// requesterID 为 0 表示游客 —— 游客也记,只是 user_id 落 0(表就是这么设计的)。
// 可见性口径与文件路由一致:看不见的视频不该被刷记录
//
// TODO(幂等): 现在是无条件追加,同一请求重发(前端重试、刷新、sendBeacon 在 pagehide
// 上重复触发)都会再插一行并把 play_count 重复加一。修法是给每次观看一个会话 id:
// 客户端上报时带上,表加 UNIQUE (user_id, session_id),插入用 ON CONFLICT DO NOTHING
// 判「是不是这次会话的第一次」,冲突则只把 watched 往大里更新(进度不倒退)。
// 索引必须带 user_id —— session_id 是客户端传的,只对它建唯一索引的话,
// 攻击者指定别人的 session_id 就能覆盖掉别人的记录
func (s *VideoService) ReportPlay(ctx context.Context, videoID, requesterID int64, req PlayReportReq, ip string) error {
	video, err := s.findVideo(ctx, videoID)
	if err != nil {
		return err
	}
	if !visibleTo(video.Status, video.AuthorID, requesterID) {
		return errs.ErrNotFound.WithMsg("视频不存在")
	}
	// 不卡这条的话,前端传 watched > duration 会算出 >100% 的完播率
	if req.Watched > req.Duration {
		return errs.ErrInvalidParam.WithMsg("观看时长不能超过视频总时长")
	}

	rec := &PlayRecord{
		UserID:   requesterID,
		VideoID:  videoID,
		AuthorID: video.AuthorID, // 冗余存一份,按作者聚合时免 JOIN
		Watched:  req.Watched,
		Duration: req.Duration,
		IP:       ip,
	}
	if err := s.videorepo.SavePlayReport(ctx, rec); err != nil {
		slog.ErrorContext(ctx, "记录播放失败", "video_id", videoID, "user_id", requesterID, "err", err)
		return errs.ErrInternal.WithMsg("记录播放失败")
	}
	return nil
}

// defaultHistoryLimit 观看历史不传 limit 时的默认页大小
const defaultHistoryLimit = 20

// ListHistory 取某用户的观看历史(GET /videos/history)。
//
// requesterID 必须 > 0 —— 路由上挂了 SetSensitive,游客根本到不了这;这里再判一次
// 是因为 user_id = 0 是「所有未登录访客」共用的一个桶,一旦放行,
// 查出来的是所有人的记录混在一起,不是「我的历史」
func (s *VideoService) ListHistory(ctx context.Context, requesterID int64, req ListHistoryReq) (*ListHistoryResp, error) {
	if requesterID <= 0 {
		return nil, errs.ErrUnauthorized.WithMsg("用户未登录")
	}

	limit := req.Limit
	if limit == 0 {
		limit = defaultHistoryLimit
	}
	// 多要一条:能取到就说明后面还有,这时才给游标(和最新流同一套判据)
	want := limit + 1

	// 游标是 (watched_at, video_id) 复合的:两个字段要么都传要么都不传,
	// 只传一个的话边界不完整,还不如当首页处理。
	// beforeVideoID 留 0 表示首页,由仓储把边界推到无穷远
	var (
		before        time.Time
		beforeVideoID int64
	)
	switch {
	case req.CursorWatchedAt == nil && req.CursorVideoID == nil:
		// 首页,不设边界
	case req.CursorWatchedAt == nil || req.CursorVideoID == nil:
		return nil, errs.ErrInvalidParam.WithMsg("分页游标无效")
	default:
		if *req.CursorVideoID <= 0 {
			return nil, errs.ErrInvalidParam.WithMsg("分页游标无效")
		}
		before = time.UnixMicro(*req.CursorWatchedAt)
		beforeVideoID = *req.CursorVideoID
	}

	entries, err := s.videorepo.ListPlayHistory(ctx, requesterID, before, beforeVideoID, want)
	if err != nil {
		slog.ErrorContext(ctx, "查询观看历史失败", "user_id", requesterID, "err", err)
		return nil, errs.ErrInternal.WithMsg("查询观看历史失败")
	}

	resp := &ListHistoryResp{Items: []HistoryItem{}} // 空页也要是 [],前端才不用判空
	if len(entries) == 0 {
		return resp, nil
	}

	// 多要的那条只用来判「还有没有更多」,在这里先截掉。
	// 用 entries 的长度判而不是拼装后的 —— 视频可能在这两次查询之间被删掉,
	// 少拼了几条就会把「还有下一页」误判成到底
	hasMore := len(entries) > limit
	if hasMore {
		entries = entries[:limit]
	}

	ids := make([]int64, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.VideoID)
	}
	videos, err := s.videorepo.FindVideosByIDs(ctx, ids)
	if err != nil {
		slog.ErrorContext(ctx, "查询观看历史里的视频失败", "user_id", requesterID, "err", err)
		return nil, errs.ErrInternal.WithMsg("查询观看历史失败")
	}
	byID := make(map[int64]*Video, len(videos))
	for _, v := range videos {
		byID[v.ID] = v
	}

	// 按 entries 的顺序拼,不是 byID 的顺序 —— IN 查询不保证返回顺序
	for _, e := range entries {
		v, ok := byID[e.VideoID]
		if !ok {
			// 两次查询之间视频被软删或改了状态,跳过这条
			continue
		}
		resp.Items = append(resp.Items, HistoryItem{
			VideoResp: *toVideoResp(v),
			Watched:   e.Watched,
			Duration:  e.Duration,
			WatchedAt: e.WatchedAt,
		})
	}
	if len(resp.Items) == 0 {
		return resp, nil
	}

	// 补点赞态,和最新流一致 —— 历史页上也要能直接点赞
	statuses, err := s.GetUserLikeStatuses(ctx, requesterID, ids)
	if err != nil {
		return nil, err
	}
	for i := range resp.Items {
		liked := statuses[resp.Items[i].ID]
		resp.Items[i].IsLike = &liked
	}

	// 只在确实还有下一页时给游标;到底了就是 nil(见 ListHistoryResp 注释)
	if hasMore {
		last := resp.Items[len(resp.Items)-1]
		resp.NextCursor = &HistoryCursor{WatchedAt: last.WatchedAt.UnixMicro(), VideoID: last.ID}
	}
	return resp, nil
}

// toVideoResp 把 entity 转成对外视图,不暴露 DeletedAt 与 Popularity
func toVideoResp(v *Video) *VideoResp {
	if v == nil {
		return nil
	}
	return &VideoResp{
		ID:           v.ID,
		AuthorID:     v.AuthorID,
		Username:     v.Username,
		AvatarURL:    v.AvatarURL,
		Title:        v.Title,
		Description:  v.Description,
		PlayURL:      v.PlayURL,
		CoverURL:     v.CoverURL,
		CreatedAt:    v.CreatedAt,
		Status:       v.Status,
		PlayCount:    v.PlayCount,
		LikesCount:   v.LikesCount,
		CommentCount: v.CommentCount,
	}
}
