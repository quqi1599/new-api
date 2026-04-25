package controller

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

var (
	errInternalAdminPermissionSameLevel   = errors.New("internal admin permission same level denied")
	errInternalAdminPermissionHigherLevel = errors.New("internal admin permission higher level denied")
	errInternalAdminInvalidParams         = errors.New("internal admin invalid params")
	errInternalAdminInputInvalid          = errors.New("internal admin input invalid")
	errInternalAdminCreateHigherLevel     = errors.New("internal admin cannot create higher level")
)

type internalAdminUpdateUserRequest struct {
	Username    *string `json:"username"`
	Password    *string `json:"password"`
	DisplayName *string `json:"display_name"`
	Group       *string `json:"group"`
	Quota       *int    `json:"quota"`
	Remark      *string `json:"remark"`
	Note        *string `json:"note"`
}

func InternalAdminListUsers(c *gin.Context) {
	pageInfo := common.GetPageQuery(c)
	users, total, err := model.GetAllUsers(pageInfo, c.Query("order"))
	if err != nil {
		common.ApiError(c, err)
		return
	}
	pageInfo.SetTotal(int(total))
	pageInfo.SetItems(users)
	common.ApiSuccess(c, pageInfo)
}

func InternalAdminSearchUsers(c *gin.Context) {
	pageInfo := common.GetPageQuery(c)
	users, total, err := model.SearchUsers(c.Query("keyword"), c.Query("group"), pageInfo.GetStartIdx(), pageInfo.GetPageSize(), c.Query("order"))
	if err != nil {
		common.ApiError(c, err)
		return
	}
	pageInfo.SetTotal(int(total))
	pageInfo.SetItems(users)
	common.ApiSuccess(c, pageInfo)
}

func InternalAdminGetUser(c *gin.Context) {
	userID, err := parseIntParam(c, "id")
	if err != nil {
		common.ApiError(c, err)
		return
	}
	user, err := getInternalAdminUserByID(c.GetInt("role"), userID)
	if err != nil {
		if errors.Is(err, errInternalAdminPermissionSameLevel) {
			common.ApiErrorI18n(c, i18n.MsgUserNoPermissionSameLevel)
			return
		}
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, user)
}

func InternalAdminCreateUser(c *gin.Context) {
	var req model.User
	if err := common.DecodeJson(c.Request.Body, &req); err != nil {
		common.ApiErrorI18n(c, i18n.MsgInvalidParams)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || req.Password == "" {
		common.ApiErrorI18n(c, i18n.MsgInvalidParams)
		return
	}
	createdUser, err := createInternalAdminUser(c.GetInt("role"), req)
	if err != nil {
		if errors.Is(err, errInternalAdminCreateHigherLevel) {
			common.ApiErrorI18n(c, i18n.MsgUserCannotCreateHigherLevel)
			return
		}
		if errors.Is(err, errInternalAdminInputInvalid) {
			common.ApiErrorI18n(c, i18n.MsgUserInputInvalid, map[string]any{"Error": err.Error()})
			return
		}
		if errors.Is(err, errInternalAdminInvalidParams) {
			common.ApiErrorI18n(c, i18n.MsgInvalidParams)
			return
		}
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, createdUser)
}

func InternalAdminUpdateUser(c *gin.Context) {
	userID, err := parseIntParam(c, "id")
	if err != nil {
		common.ApiError(c, err)
		return
	}
	var req internalAdminUpdateUserRequest
	if err = common.DecodeJson(c.Request.Body, &req); err != nil {
		common.ApiErrorI18n(c, i18n.MsgInvalidParams)
		return
	}
	updatedUser, err := updateInternalAdminUser(c.GetInt("role"), userID, req)
	if err != nil {
		if errors.Is(err, errInternalAdminPermissionHigherLevel) {
			common.ApiErrorI18n(c, i18n.MsgUserNoPermissionHigherLevel)
			return
		}
		if errors.Is(err, errInternalAdminInputInvalid) {
			common.ApiErrorI18n(c, i18n.MsgUserInputInvalid, map[string]any{"Error": err.Error()})
			return
		}
		if errors.Is(err, errInternalAdminInvalidParams) {
			common.ApiErrorI18n(c, i18n.MsgInvalidParams)
			return
		}
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, updatedUser)
}

func InternalAdminDeleteUser(c *gin.Context) {
	userID, err := parseIntParam(c, "id")
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if err = deleteInternalAdminUser(c.GetInt("role"), userID); err != nil {
		if errors.Is(err, errInternalAdminPermissionHigherLevel) {
			common.ApiErrorI18n(c, i18n.MsgUserNoPermissionHigherLevel)
			return
		}
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"id": userID})
}

func parseIntParam(c *gin.Context, key string) (int, error) {
	return strconv.Atoi(c.Param(key))
}

func getInternalAdminUserByID(operatorRole int, userID int) (*model.User, error) {
	user, err := model.GetUserById(userID, false)
	if err != nil {
		return nil, err
	}
	if operatorRole <= user.Role && operatorRole != common.RoleRootUser {
		return nil, errInternalAdminPermissionSameLevel
	}
	return user, nil
}

func createInternalAdminUser(operatorRole int, req model.User) (*model.User, error) {
	if req.DisplayName == "" {
		req.DisplayName = req.Username
	}
	if req.Role >= operatorRole {
		return nil, errInternalAdminCreateHigherLevel
	}
	cleanUser := model.User{
		Username:    req.Username,
		Password:    req.Password,
		DisplayName: strings.TrimSpace(req.DisplayName),
		Role:        req.Role,
	}
	if cleanUser.Username == "" || cleanUser.Password == "" {
		return nil, errInternalAdminInvalidParams
	}
	if err := common.Validate.Struct(&cleanUser); err != nil {
		return nil, fmt.Errorf("%w: %w", errInternalAdminInputInvalid, err)
	}
	if err := cleanUser.Insert(0); err != nil {
		return nil, err
	}
	return model.GetUserById(cleanUser.Id, false)
}

func updateInternalAdminUser(operatorRole int, userID int, req internalAdminUpdateUserRequest) (*model.User, error) {
	if userID == 0 {
		return nil, errInternalAdminInvalidParams
	}
	originUser, err := model.GetUserById(userID, false)
	if err != nil {
		return nil, err
	}
	if operatorRole <= originUser.Role && operatorRole != common.RoleRootUser {
		return nil, errInternalAdminPermissionHigherLevel
	}

	updatedUser, err := model.GetUserById(userID, true)
	if err != nil {
		return nil, err
	}

	if req.Username != nil {
		updatedUser.Username = strings.TrimSpace(*req.Username)
	}
	if req.Password != nil {
		updatedUser.Password = *req.Password
	} else {
		updatedUser.Password = "$I_LOVE_U"
	}
	if req.DisplayName != nil {
		updatedUser.DisplayName = strings.TrimSpace(*req.DisplayName)
	}
	if req.Group != nil {
		updatedUser.Group = strings.TrimSpace(*req.Group)
	}
	if req.Quota != nil {
		updatedUser.Quota = *req.Quota
	}
	if req.Remark != nil {
		updatedUser.Remark = *req.Remark
	}

	quotaChanged := originUser.Quota != updatedUser.Quota
	targetUserID := updatedUser.Id
	targetQuota := updatedUser.Quota

	if err := common.Validate.Struct(updatedUser); err != nil {
		return nil, fmt.Errorf("%w: %w", errInternalAdminInputInvalid, err)
	}
	if updatedUser.Password == "$I_LOVE_U" {
		updatedUser.Password = ""
	}

	if err := updatedUser.Edit(updatedUser.Password != ""); err != nil {
		return nil, err
	}
	if quotaChanged {
		if err := model.DB.Model(&model.User{}).Where("id = ?", targetUserID).Update("quota", targetQuota).Error; err != nil {
			return nil, err
		}
		if err := model.InvalidateUserCache(targetUserID); err != nil {
			common.SysLog("failed to invalidate user cache: " + err.Error())
		}
		model.RecordLog(originUser.Id, model.LogTypeManage, formatInternalAdminQuotaChangeLog(originUser.Quota, targetQuota, req.Note))
	}
	return model.GetUserById(targetUserID, true)
}

func deleteInternalAdminUser(operatorRole int, userID int) error {
	originUser, err := model.GetUserById(userID, false)
	if err != nil {
		return err
	}
	if operatorRole <= originUser.Role {
		return errInternalAdminPermissionHigherLevel
	}
	return model.HardDeleteUserById(userID)
}

func formatInternalAdminQuotaChangeLog(beforeQuota int, afterQuota int, note *string) string {
	message := fmt.Sprintf("管理员将用户额度从 %s修改为 %s", logger.LogQuota(beforeQuota), logger.LogQuota(afterQuota))
	if note == nil {
		return message
	}
	trimmed := strings.TrimSpace(*note)
	if trimmed == "" {
		return message
	}
	return fmt.Sprintf("%s，备注：%s", message, trimmed)
}
