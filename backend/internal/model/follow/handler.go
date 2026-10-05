package follow

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"feed-system/internal/middleware"
	"feed-system/internal/pkg/errs"
	"feed-system/internal/pkg/response"
)

// FollowHandler 关注模块 HTTP handler
type FollowHandler struct {
	svc *FollowService
}

// NewFollowHandler 构造 FollowHandler
func NewFollowHandler(svc *FollowService) *FollowHandler {
	return &FollowHandler{svc: svc}
}

// followeeIDParam 取路径里的 :id(这里是被关注者)。转不出 int64 就是客户端传了脏值
func followeeIDParam(c *gin.Context) (int64, error) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errs.ErrInvalidParam.WithMsg("用户 ID 无效")
	}
	return id, nil
}

// Follow 关注某人(POST /users/:id/follow)
func (h *FollowHandler) Follow(c *gin.Context) {
	userID := middleware.UserID(c)
	if userID == 0 {
		response.Error(c, errs.ErrUnauthorized)
		return
	}

	followeeID, err := followeeIDParam(c)
	if err != nil {
		response.Error(c, err)
		return
	}

	resp, err := h.svc.Follow(c.Request.Context(), userID, followeeID)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}

// Unfollow 取关某人(DELETE /users/:id/follow)
func (h *FollowHandler) Unfollow(c *gin.Context) {
	userID := middleware.UserID(c)
	if userID == 0 {
		response.Error(c, errs.ErrUnauthorized)
		return
	}

	followeeID, err := followeeIDParam(c)
	if err != nil {
		response.Error(c, err)
		return
	}

	resp, err := h.svc.Unfollow(c.Request.Context(), userID, followeeID)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}

// ListFollowees 我关注的人(GET /users/me/followees)。
// 没有路径参数,只认「当前登录用户」,所以 userID 就取自鉴权注入
func (h *FollowHandler) ListFollowees(c *gin.Context) {
	userID := middleware.UserID(c)
	if userID == 0 {
		response.Error(c, errs.ErrUnauthorized)
		return
	}

	resp, err := h.svc.ListFollowees(c.Request.Context(), userID)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}

// ListFollowers 谁关注了我(GET /users/me/followers)。同样只认当前登录用户
func (h *FollowHandler) ListFollowers(c *gin.Context) {
	userID := middleware.UserID(c)
	if userID == 0 {
		response.Error(c, errs.ErrUnauthorized)
		return
	}

	var req ListFollowersReq
	if err := c.ShouldBindQuery(&req); err != nil {
		response.Error(c, errs.ErrInvalidParam.WithMsg("查询参数无效"))
		return
	}

	resp, err := h.svc.ListFollowers(c.Request.Context(), userID, req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}
