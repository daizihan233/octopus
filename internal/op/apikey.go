package op

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/utils/cache"
	"gorm.io/gorm"
)

var apiKeyCache = cache.New[int, model.APIKey](16)
var apiKeyIDMap = cache.New[string, int](16)

var (
	// ErrAPIKeyExists 表示自定义的 API Key 与已有 Key 重复
	ErrAPIKeyExists = errors.New("API key already exists")
	// ErrAPIKeyEmpty 表示 API Key 为空
	ErrAPIKeyEmpty = errors.New("API key cannot be empty")
	// ErrAPIKeyTooShort 表示自定义的 API Key 长度不足
	ErrAPIKeyTooShort = errors.New("API key is too short")
	// ErrAPIKeyTooLong 表示自定义的 API Key 长度超出限制
	ErrAPIKeyTooLong = errors.New("API key is too long")
)

// minAPIKeyLength 自定义 API Key 的最小长度，避免弱 key 被暴力枚举
const minAPIKeyLength = 16

// maxAPIKeyLength 自定义 API Key 的最大长度，避免超长 key 触发 DB 字段溢出
const maxAPIKeyLength = 128

func APIKeyCreate(key *model.APIKey, ctx context.Context) error {
	key.APIKey = strings.TrimSpace(key.APIKey)
	if key.APIKey == "" {
		return ErrAPIKeyEmpty
	}
	if len(key.APIKey) < minAPIKeyLength {
		return ErrAPIKeyTooShort
	}
	if len(key.APIKey) > maxAPIKeyLength {
		return ErrAPIKeyTooLong
	}
	if err := checkAPIKeyUnique(key.APIKey, 0); err != nil {
		return err
	}
	if err := db.GetDB().WithContext(ctx).Create(key).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			// DB 层唯一索引冲突（并发窗口/多实例时内存检查可能漏判），映射为重复错误
			return ErrAPIKeyExists
		}
		return fmt.Errorf("failed to create API key: %w", err)
	}
	apiKeyCache.Set(key.ID, *key)
	apiKeyIDMap.Set(key.APIKey, key.ID)
	return nil
}

func APIKeyUpdate(key *model.APIKey, ctx context.Context) error {
	existing, ok := apiKeyCache.Get(key.ID)
	if !ok {
		return fmt.Errorf("API key not found")
	}
	// 传入空字符串表示不修改 API Key
	if key.APIKey != "" {
		key.APIKey = strings.TrimSpace(key.APIKey)
		if key.APIKey == "" {
			return ErrAPIKeyEmpty
		}
		if len(key.APIKey) < minAPIKeyLength {
			return ErrAPIKeyTooShort
		}
		if len(key.APIKey) > maxAPIKeyLength {
			return ErrAPIKeyTooLong
		}
		if err := checkAPIKeyUnique(key.APIKey, key.ID); err != nil {
			return err
		}
	} else {
		key.APIKey = existing.APIKey
	}
	if err := db.GetDB().WithContext(ctx).Save(key).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return ErrAPIKeyExists
		}
		return fmt.Errorf("failed to update API key: %w", err)
	}
	if key.APIKey != existing.APIKey {
		apiKeyIDMap.Del(existing.APIKey)
		apiKeyIDMap.Set(key.APIKey, key.ID)
	}
	apiKeyCache.Set(key.ID, *key)
	return nil
}

func APIKeyList(ctx context.Context) ([]model.APIKey, error) {
	keys := make([]model.APIKey, 0, apiKeyCache.Len())
	for _, apiKey := range apiKeyCache.GetAll() {
		keys = append(keys, apiKey)
	}
	return keys, nil
}

func APIKeyGet(id int, ctx context.Context) (model.APIKey, error) {
	apiKey, ok := apiKeyCache.Get(id)
	if !ok {
		return model.APIKey{}, fmt.Errorf("API key not found")
	}
	return apiKey, nil
}

// checkAPIKeyUnique 校验 apiKey 是否与其它 API Key 重复，excludeID 用于更新时排除自身
func checkAPIKeyUnique(apiKey string, excludeID int) error {
	if id, ok := apiKeyIDMap.Get(apiKey); ok && id != excludeID {
		return ErrAPIKeyExists
	}
	return nil
}

func APIKeyGetByAPIKey(apiKey string, ctx context.Context) (model.APIKey, error) {
	id, ok := apiKeyIDMap.Get(apiKey)
	if !ok {
		return model.APIKey{}, fmt.Errorf("API key not found")
	}
	return APIKeyGet(id, ctx)
}

func APIKeyDelete(id int, ctx context.Context) error {
	existing, ok := apiKeyCache.Get(id)
	if err := StatsAPIKeyDel(id); err != nil {
		return fmt.Errorf("failed to delete stats API key: %v", err)
	}
	result := db.GetDB().WithContext(ctx).Delete(&model.APIKey{ID: id})
	if result.RowsAffected == 0 {
		return fmt.Errorf("API key not found")
	}
	if result.Error != nil {
		return fmt.Errorf("failed to delete API key: %w", result.Error)
	}
	apiKeyCache.Del(id)
	if ok {
		apiKeyIDMap.Del(existing.APIKey)
	}
	return nil
}

func apiKeyRefreshCache(ctx context.Context) error {
	apiKeys := []model.APIKey{}
	if err := db.GetDB().WithContext(ctx).Find(&apiKeys).Error; err != nil {
		return err
	}
	for _, apiKey := range apiKeys {
		apiKeyCache.Set(apiKey.ID, apiKey)
		apiKeyIDMap.Set(apiKey.APIKey, apiKey.ID)
	}
	return nil
}
