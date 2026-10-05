package feed

import (
	"github.com/gin-gonic/gin"

	"feed-system/internal/middleware"
	"feed-system/internal/pkg/errs"
	"feed-system/internal/pkg/response"
)

type FeedHandler struct {
	svc *FeedService
}

func NewFeedHandler(svc *FeedService) *FeedHandler {
	return &FeedHandler{svc: svc}
}

func (h *FeedHandler) ListLatest(c *gin.Context) {
	var req ListLatestReq
	if err := c.ShouldBindQuery(&req); err != nil {
		response.Error(c, errs.ErrInvalidParam.WithMsg("查询参数无效"))
		return
	}

	resp, err := h.svc.ListLatest(c.Request.Context(), req, middleware.UserID(c))
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}

func (h *FeedHandler) ListFollowing(c *gin.Context) {
	var req ListFollowingReq
	if err := c.ShouldBindQuery(&req); err != nil {
		response.Error(c, errs.ErrInvalidParam.WithMsg("查询参数无效"))
		return
	}

	resp, err := h.svc.ListFollowing(c.Request.Context(), req, middleware.UserID(c))
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}
