package video

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"feed-system/internal/middleware"
	"feed-system/internal/pkg/errs"
	"feed-system/internal/pkg/response"
)

// maxChunkBytes 单个分片请求体积上限。贴着 VideoChunkSize 定,只给 multipart
// boundary 与各字段头留余量 —— 卡松了等于没卡:超出部分 stdlib 照单全收,落哪由不得你
const maxChunkBytes = VideoChunkSize + 1<<20

// chunkParseError 把 multipart 解析错误翻译成业务错误。必须单独认出 *http.MaxBytesError ——
// 「分片太大」和「请求格式不对」对前端是两件事:前者调小分片,后者是 bug
func chunkParseError(err error) error {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return errs.ErrInvalidParam.WithMsg("分片超过大小上限")
	}
	return errs.ErrInvalidParam.WithMsg("分片请求格式不正确")
}

type VideoHandler struct {
	svc *VideoService
}

func NewVideoHandler(svc *VideoService) *VideoHandler {
	return &VideoHandler{svc: svc}
}

func (v *VideoHandler) InitChunkUpload(c *gin.Context) {
	userID := middleware.UserID(c)
	if userID == 0 {
		response.Error(c, errs.ErrUnauthorized)
		return
	}

	var req InitChunkUploadRequest
	err := c.ShouldBindBodyWithJSON(&req)
	if err != nil {
		response.Error(c, errs.ErrInvalidParam)
		return
	}

	resp, err := v.svc.InitChunkUpload(c.Request.Context(), req, userID)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}

func (v *VideoHandler) UploadChunk(c *gin.Context) {
	userID := middleware.UserID(c)
	if userID == 0 {
		response.Error(c, errs.ErrUnauthorized)
		return
	}

	// 必须在任何读 body 的操作之前 —— c.FormFile / c.ShouldBind 内部会
	// ParseMultipartForm 把整个 body 读进来,晚一行就晚了
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxChunkBytes)

	var req UploadChunkRequest
	if err := c.ShouldBind(&req); err != nil {
		response.Error(c, chunkParseError(err))
		return
	}

	// form 字段和文件同属一个 multipart body,ParseMultipartForm 只跑一次,再取文件不会重复读 body
	fileheader, err := c.FormFile("chunk")
	if err != nil {
		response.Error(c, chunkParseError(err))
		return
	}

	resp, err := v.svc.UploadChunk(c.Request.Context(), req, userID, fileheader)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}

func (v *VideoHandler) CompleteChunkUpload(c *gin.Context) {
	userID := middleware.UserID(c)
	if userID == 0 {
		response.Error(c, errs.ErrUnauthorized)
		return
	}

	// 这个接口只传元数据、没有文件,所以走 JSON 而不是 multipart
	var req CompleteChunkUploadReq
	if err := c.ShouldBindBodyWithJSON(&req); err != nil {
		response.Error(c, errs.ErrInvalidParam)
		return
	}

	resp, err := v.svc.CompleteChunkUpload(c.Request.Context(), req, userID)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}

// videoIDParam 取路径里的 :id。转不出 int64 就是客户端传了脏值
func videoIDParam(c *gin.Context) (int64, error) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errs.ErrInvalidParam.WithMsg("视频 ID 无效")
	}
	return id, nil
}

func (v *VideoHandler) UpdateVideo(c *gin.Context) {
	userID := middleware.UserID(c)
	if userID == 0 {
		response.Error(c, errs.ErrUnauthorized)
		return
	}
	videoID, err := videoIDParam(c)
	if err != nil {
		response.Error(c, err)
		return
	}

	var req UpdateVideoReq
	if err := c.ShouldBindBodyWithJSON(&req); err != nil {
		response.Error(c, errs.ErrInvalidParam)
		return
	}

	resp, err := v.svc.UpdateVideo(c.Request.Context(), videoID, userID, req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}

func (v *VideoHandler) PublishVideo(c *gin.Context) {
	userID := middleware.UserID(c)
	if userID == 0 {
		response.Error(c, errs.ErrUnauthorized)
		return
	}
	videoID, err := videoIDParam(c)
	if err != nil {
		response.Error(c, err)
		return
	}

	resp, err := v.svc.PublishVideo(c.Request.Context(), videoID, userID)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}

func (v *VideoHandler) GetVideoDetail(c *gin.Context) {
	videoID, err := videoIDParam(c)
	if err != nil {
		response.Error(c, err)
		return
	}

	// 软鉴权:匿名没 token 时 UserID 返回 0,可见范围交给 service 决定
	resp, err := v.svc.GetVideoDetail(c.Request.Context(), videoID, middleware.UserID(c))
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}

func (v *VideoHandler) ReportPlay(c *gin.Context) {
	videoID, err := videoIDParam(c)
	if err != nil {
		response.Error(c, err)
		return
	}

	var req PlayReportReq
	if err := c.ShouldBindBodyWithJSON(&req); err != nil {
		response.Error(c, errs.ErrInvalidParam)
		return
	}

	// 软鉴权:游客 UserID 为 0,照样记录。不像上传那几个接口那样拦 0 —— 那是全项目
	// 唯一「未登录也算合法」的写入口。
	// ip 取 RemoteIP 不取 ClientIP:后者信任 X-Forwarded-For,那条头是客户端能自己填的
	err = v.svc.ReportPlay(c.Request.Context(), videoID, middleware.UserID(c), req, c.RemoteIP())
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, nil)
}

// ListHistory 观看历史。只给当前登录用户看自己的 —— 路由上挂了 SetSensitive,
// 没 token 在中间件就被拦成 401,走不到这里
func (v *VideoHandler) ListHistory(c *gin.Context) {
	var req ListHistoryReq
	if err := c.ShouldBindQuery(&req); err != nil {
		response.Error(c, errs.ErrInvalidParam.WithMsg("查询参数无效"))
		return
	}

	resp, err := v.svc.ListHistory(c.Request.Context(), middleware.UserID(c), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}

func (v *VideoHandler) Like(c *gin.Context) {
	userID := middleware.UserID(c)
	if userID == 0 {
		response.Error(c, errs.ErrUnauthorized)
		return
	}
	videoID, err := videoIDParam(c)
	if err != nil {
		response.Error(c, err)
		return
	}

	resp, err := v.svc.LikeVideo(c.Request.Context(), videoID, userID)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}

func (v *VideoHandler) Unlike(c *gin.Context) {
	userID := middleware.UserID(c)
	if userID == 0 {
		response.Error(c, errs.ErrUnauthorized)
		return
	}
	videoID, err := videoIDParam(c)
	if err != nil {
		response.Error(c, err)
		return
	}

	resp, err := v.svc.UnlikeVideo(c.Request.Context(), videoID, userID)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}

func (v *VideoHandler) DeleteVideo(c *gin.Context) {
	userID := middleware.UserID(c)
	if userID == 0 {
		response.Error(c, errs.ErrUnauthorized)
		return
	}
	videoID, err := videoIDParam(c)
	if err != nil {
		response.Error(c, err)
		return
	}

	if err := v.svc.DeleteVideo(c.Request.Context(), videoID, userID); err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, nil)
}

// ListUserVideos 某人的视频列表(GET /users/:id/videos)。
// 路径里的 :id 是作者 ID 不是视频 ID,所以不能用 videoIDParam(它的报错文案写死了「视频 ID」)
func (v *VideoHandler) ListUserVideos(c *gin.Context) {
	authorID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || authorID <= 0 {
		response.Error(c, errs.ErrInvalidParam.WithMsg("用户 ID 无效"))
		return
	}

	var req ListUserVideosReq
	if err := c.ShouldBindQuery(&req); err != nil {
		response.Error(c, errs.ErrInvalidParam.WithMsg("查询参数无效"))
		return
	}

	// 软鉴权:游客 UserID 为 0,只看得到已发布;作者本人额外能看到自己的草稿
	resp, err := v.svc.ListUserVideos(c.Request.Context(), authorID, middleware.UserID(c), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}
