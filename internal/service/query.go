package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/store"
)

// 查询参数与可独立调用的查询入口支持的唯四参数。
const (
	paramNamespace = "namespace"
	paramName      = "name"
	paramQueue     = "queue"
	paramNode      = "node"
)

// ErrInvalidPlacementQuery 表示查询参数不合法：未知键、值数量不是一个、
// 空字符串或带 name 时的非法组合。它与 HTTP 无关，可被 errors.Is 识别。
var ErrInvalidPlacementQuery = errors.New("invalid placement query")

// ErrPlacementNotFound 表示带 name 的单条查询没有命中已提交记录。
var ErrPlacementNotFound = errors.New("placement not found")

// QueryResult 是 Query 的结果：单条命中时仅 Record 非 nil；列表成功时仅
// Items 非 nil（无匹配是非 nil 空切片）；失败时两者均为 nil。
type QueryResult struct {
	Record *store.Placement
	Items  []*store.Placement
}

var allowedQueryParams = map[string]bool{
	paramNamespace: true,
	paramName:      true,
	paramQueue:     true,
	paramNode:      true,
}

// Query 运行与 GET /v1/placements 完全相同的查询判断，且不依赖 Gin 或
// HTTP 对象：调用方传入已解析、保留重复值的 url.Values（例如
// url.ParseQuery 的返回值），Query 不改写它。
//
// 带 name 时只能同时提供 namespace，命中返回原记录，不存在返回
// ErrPlacementNotFound；不带 name 时，namespace/queue/node 按交集筛选，
// 未提供的条件忽略，nil 或空集合查询全部。未知键、值数量不是一个、空
// 字符串及非法组合统一返回 ErrInvalidPlacementQuery，校验失败不访问存储。
// 值必须已经完成百分号解码，Query 不修剪空白也不再次解码；合法查询遇到
// 存储失败返回 ErrStorageUnavailable。
func (s *Service) Query(ctx context.Context, values url.Values) (*QueryResult, error) {
	params, err := validateQuery(values)
	if err != nil {
		return nil, err
	}

	if name := params[paramName]; name != "" {
		rec, getErr := s.st.Get(ctx, params[paramNamespace], name)
		if getErr != nil {
			if errors.Is(getErr, store.ErrNotFound) {
				return nil, ErrPlacementNotFound
			}
			return nil, queryStorageError(getErr)
		}
		return &QueryResult{Record: rec}, nil
	}

	recs, listErr := s.st.List(ctx, store.ListFilter{
		Namespace: params[paramNamespace],
		Queue:     params[paramQueue],
		Node:      params[paramNode],
	})
	if listErr != nil {
		return nil, queryStorageError(listErr)
	}
	if recs == nil {
		recs = []*store.Placement{}
	}
	return &QueryResult{Items: recs}, nil
}

// validateQuery 只接受四种已知参数且每个键恰好一个非空值，并检查 name
// 只能与 namespace 同时出现。纯内存校验，失败时不访问存储，也不修改入参。
func validateQuery(values url.Values) (map[string]string, error) {
	params := make(map[string]string, len(values))
	for key, vals := range values {
		if !allowedQueryParams[key] || len(vals) != 1 || vals[0] == "" {
			return nil, ErrInvalidPlacementQuery
		}
		params[key] = vals[0]
	}
	if _, hasName := params[paramName]; hasName {
		if _, hasNamespace := params[paramNamespace]; !hasNamespace {
			return nil, ErrInvalidPlacementQuery
		}
		if _, hasQueue := params[paramQueue]; hasQueue {
			return nil, ErrInvalidPlacementQuery
		}
		if _, hasNode := params[paramNode]; hasNode {
			return nil, ErrInvalidPlacementQuery
		}
	}
	return params, nil
}

func queryStorageError(err error) error {
	return fmt.Errorf("query placements: %w: %w", ErrStorageUnavailable, err)
}
