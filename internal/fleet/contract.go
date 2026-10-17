// Package fleet 实现按用户分区的工具契约合并（Fleet merge）。
// 语义照搬 nexusdock internal/agentdock 的 tool_contract + PublishedToolBridge：
//   - ToolContractHash 提取影响校验结果的字段做语义哈希，规范化无序集合；
//   - mergeFleetToolDescriptors 合并同名工具（required/_meta 一致、可选属性并集）；
//   - PublishedBridge 维护每用户命名空间的公开契约，首发布持久化先行，
//     无 provider 时下架，调用前复核目标节点契约在兼容集合内。
package fleet

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	protocol "github.com/uvwt/agentdock-protocol"
)

const maxToolContractDifferences = 5

// ErrIncompatibleToolContract 表示不同 provider 的工具契约无法合并成同一代公开契约。
var ErrIncompatibleToolContract = errors.New("工具契约不可安全合并")

// ContractDifference 描述公开契约与节点现场契约之间的单点差异。
type ContractDifference struct {
	Path      string `json:"path"`
	Published any    `json:"published"`
	Node      any    `json:"node"`
}

// ContractMismatch 是调用时发现节点契约不在兼容集合内的结构化详情。
type ContractMismatch struct {
	Code          string               `json:"code"`
	Message       string               `json:"message"`
	Tool          string               `json:"tool"`
	NodeID        string               `json:"node_id"`
	PublishedHash string               `json:"published_hash"`
	NodeHash      string               `json:"node_hash"`
	Differences   []ContractDifference `json:"differences,omitempty"`
}

type comparableContract struct {
	InputSchema  map[string]any `json:"inputSchema"`
	OutputSchema map[string]any `json:"outputSchema,omitempty"`
	Meta         map[string]any `json:"_meta,omitempty"`
}

// ToolContractHash 计算工具描述符的语义契约哈希。
func ToolContractHash(descriptor protocol.ToolDescriptor) (string, error) {
	contract, err := comparableOf(descriptor)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(contract)
	if err != nil {
		return "", fmt.Errorf("编码工具 %s 契约: %w", descriptor.Name, err)
	}
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", sum), nil
}

func comparableOf(descriptor protocol.ToolDescriptor) (comparableContract, error) {
	inputSchema, err := semanticSchemaMap(descriptor.InputSchema)
	if err != nil {
		return comparableContract{}, fmt.Errorf("规范化工具 %s 输入契约: %w", descriptor.Name, err)
	}
	outputSchema, err := semanticSchemaMap(descriptor.OutputSchema)
	if err != nil {
		return comparableContract{}, fmt.Errorf("规范化工具 %s 输出契约: %w", descriptor.Name, err)
	}
	return comparableContract{
		InputSchema: inputSchema, OutputSchema: outputSchema,
		Meta: executionToolMeta(descriptor.Meta),
	}, nil
}

func executionToolMeta(meta map[string]any) map[string]any {
	if len(meta) == 0 {
		return nil
	}
	execution := make(map[string]any, len(meta))
	for key, value := range meta {
		if key == "ui" {
			continue
		}
		execution[key] = value
	}
	if len(execution) == 0 {
		return nil
	}
	return execution
}

// semanticSchemaMap 去掉不影响校验结果的展示字段（description/title），
// 规范化 JSON Schema 中无顺序语义的集合；其余关键字原样保留。
func semanticSchemaMap(schema map[string]any) (map[string]any, error) {
	if schema == nil {
		return nil, nil
	}
	normalized, err := normalizeJSONValue(schema)
	if err != nil {
		return nil, err
	}
	value, ok := normalized.(map[string]any)
	if !ok {
		return nil, errors.New("schema 不是 JSON object")
	}
	return semanticSchemaObject(value), nil
}

func normalizeJSONValue(value any) (any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var normalized any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return nil, err
	}
	return normalized, nil
}

func semanticSchemaObject(schema map[string]any) map[string]any {
	result := make(map[string]any, len(schema))
	for key, value := range schema {
		if key == "description" || key == "title" {
			continue
		}
		normalized := semanticSchemaKeywordValue(key, value)
		if key == "required" {
			if items, ok := normalized.([]any); ok && len(items) == 0 {
				continue
			}
		}
		result[key] = normalized
	}
	return result
}

func semanticSchemaKeywordValue(key string, value any) any {
	switch key {
	case "properties", "patternProperties", "$defs", "definitions", "dependentSchemas":
		mapping, ok := value.(map[string]any)
		if !ok {
			return value
		}
		result := make(map[string]any, len(mapping))
		for name, child := range mapping {
			if schema, ok := child.(map[string]any); ok {
				result[name] = semanticSchemaObject(schema)
			} else {
				result[name] = child
			}
		}
		return result
	case "items", "contains", "not", "if", "then", "else", "propertyNames", "additionalProperties", "unevaluatedProperties":
		if schema, ok := value.(map[string]any); ok {
			return semanticSchemaObject(schema)
		}
		return value
	case "allOf", "anyOf", "oneOf", "prefixItems":
		items, ok := value.([]any)
		if !ok {
			return value
		}
		result := make([]any, len(items))
		for index, child := range items {
			if schema, ok := child.(map[string]any); ok {
				result[index] = semanticSchemaObject(schema)
			} else {
				result[index] = child
			}
		}
		return result
	case "required", "enum":
		items, ok := value.([]any)
		if !ok {
			return value
		}
		result := append([]any(nil), items...)
		sort.Slice(result, func(i, j int) bool {
			left, _ := json.Marshal(result[i])
			right, _ := json.Marshal(result[j])
			return bytes.Compare(left, right) < 0
		})
		return result
	default:
		return value
	}
}

// ContractDifferences 对比公开契约与节点现场契约（供 MCP 错误回显，上限 5 条）。
func ContractDifferences(published, node protocol.ToolDescriptor) []ContractDifference {
	publishedValue, publishedOK := normalizedContractValue(published)
	nodeValue, nodeOK := normalizedContractValue(node)
	if !publishedOK || !nodeOK {
		return nil
	}
	differences := make([]ContractDifference, 0, maxToolContractDifferences)
	collectDifferences("", publishedValue, nodeValue, &differences)
	return differences
}

func normalizedContractValue(descriptor protocol.ToolDescriptor) (map[string]any, bool) {
	contract, err := comparableOf(descriptor)
	if err != nil {
		return nil, false
	}
	encoded, err := json.Marshal(contract)
	if err != nil {
		return nil, false
	}
	var value map[string]any
	if json.Unmarshal(encoded, &value) != nil {
		return nil, false
	}
	return value, true
}

func collectDifferences(path string, published, node any, differences *[]ContractDifference) {
	if len(*differences) >= maxToolContractDifferences || reflect.DeepEqual(published, node) {
		return
	}
	publishedMap, publishedIsMap := published.(map[string]any)
	nodeMap, nodeIsMap := node.(map[string]any)
	if publishedIsMap && nodeIsMap {
		keys := unionMapKeys(publishedMap, nodeMap)
		for _, key := range keys {
			nextPath := key
			if path != "" {
				nextPath = path + "." + key
			}
			publishedValue, publishedExists := publishedMap[key]
			nodeValue, nodeExists := nodeMap[key]
			if !publishedExists || !nodeExists {
				*differences = append(*differences, ContractDifference{Path: nextPath, Published: publishedValue, Node: nodeValue})
			} else {
				collectDifferences(nextPath, publishedValue, nodeValue, differences)
			}
			if len(*differences) >= maxToolContractDifferences {
				return
			}
		}
		return
	}
	*differences = append(*differences, ContractDifference{Path: path, Published: published, Node: node})
}

// MergeFleetToolDescriptors 合并多个 provider 的同名工具契约。
// required 集合与执行级 _meta 必须一致；可选属性取并集；
// 返回全部 provider 变体哈希作为兼容集合。
func MergeFleetToolDescriptors(descriptors []protocol.ToolDescriptor) (protocol.ToolDescriptor, []string, error) {
	if len(descriptors) == 0 {
		return protocol.ToolDescriptor{}, nil, fmt.Errorf("%w: 没有 provider", ErrIncompatibleToolContract)
	}
	merged, err := cloneToolDescriptor(descriptors[0])
	if err != nil {
		return protocol.ToolDescriptor{}, nil, err
	}
	acceptedHashes := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if descriptor.Name != merged.Name {
			return protocol.ToolDescriptor{}, nil, fmt.Errorf("%w: tool name %q != %q", ErrIncompatibleToolContract, descriptor.Name, merged.Name)
		}
		hash, err := ToolContractHash(descriptor)
		if err != nil {
			return protocol.ToolDescriptor{}, nil, err
		}
		acceptedHashes = append(acceptedHashes, hash)
	}
	for _, descriptor := range descriptors[1:] {
		if !jsonValuesEqual(executionToolMeta(merged.Meta), executionToolMeta(descriptor.Meta)) {
			return protocol.ToolDescriptor{}, nil, fmt.Errorf("%w: %s execution meta", ErrIncompatibleToolContract, merged.Name)
		}
		merged.InputSchema, err = mergeSchemaMaps("inputSchema", merged.InputSchema, descriptor.InputSchema)
		if err != nil {
			return protocol.ToolDescriptor{}, nil, err
		}
		merged.OutputSchema, err = mergeSchemaMaps("outputSchema", merged.OutputSchema, descriptor.OutputSchema)
		if err != nil {
			return protocol.ToolDescriptor{}, nil, err
		}
	}
	merged.Meta = mergeFleetToolMeta(descriptors)
	merged.Annotations = mergeFleetToolAnnotations(descriptors)
	return merged, normalizeSemanticHashes(acceptedHashes), nil
}

func mergeFleetToolMeta(descriptors []protocol.ToolDescriptor) map[string]any {
	if len(descriptors) == 0 || len(descriptors[0].Meta) == 0 {
		return nil
	}
	common := make(map[string]any, len(descriptors[0].Meta))
	for key, value := range descriptors[0].Meta {
		common[key] = value
	}
	for _, descriptor := range descriptors[1:] {
		for key, value := range common {
			other, ok := descriptor.Meta[key]
			if !ok || !jsonValuesEqual(value, other) {
				delete(common, key)
			}
		}
	}
	if len(common) == 0 {
		return nil
	}
	return common
}

// mergeFleetToolAnnotations 按 MCP 默认值保守合并注解：
// 只在所有 provider 都给出更安全保证时收紧，否则保持副作用/开放世界假设。
func mergeFleetToolAnnotations(descriptors []protocol.ToolDescriptor) map[string]any {
	hasAnnotations := false
	for _, descriptor := range descriptors {
		if len(descriptor.Annotations) > 0 {
			hasAnnotations = true
			break
		}
	}
	if !hasAnnotations {
		return nil
	}
	readOnly := true
	idempotent := true
	destructive := false
	openWorld := false
	for _, descriptor := range descriptors {
		annotations := descriptor.Annotations
		readOnly = readOnly && annotationBool(annotations, "readOnlyHint", false)
		idempotent = idempotent && annotationBool(annotations, "idempotentHint", false)
		destructive = destructive || annotationBool(annotations, "destructiveHint", true)
		openWorld = openWorld || annotationBool(annotations, "openWorldHint", true)
	}
	merged := mergeCommonValues(descriptors, func(d protocol.ToolDescriptor) map[string]any { return d.Annotations })
	if merged == nil {
		merged = make(map[string]any, 4)
	}
	merged["readOnlyHint"] = readOnly
	merged["destructiveHint"] = destructive
	merged["idempotentHint"] = idempotent
	merged["openWorldHint"] = openWorld
	return merged
}

func mergeCommonValues(descriptors []protocol.ToolDescriptor, pick func(protocol.ToolDescriptor) map[string]any) map[string]any {
	if len(descriptors) == 0 {
		return nil
	}
	first := pick(descriptors[0])
	if len(first) == 0 {
		return nil
	}
	common := make(map[string]any, len(first))
	for key, value := range first {
		common[key] = value
	}
	for _, descriptor := range descriptors[1:] {
		other := pick(descriptor)
		for key, value := range common {
			otherValue, ok := other[key]
			if !ok || !jsonValuesEqual(value, otherValue) {
				delete(common, key)
			}
		}
	}
	if len(common) == 0 {
		return nil
	}
	return common
}

func annotationBool(annotations map[string]any, key string, defaultValue bool) bool {
	value, ok := annotations[key]
	if !ok || value == nil {
		return defaultValue
	}
	parsed, ok := value.(bool)
	if !ok {
		return defaultValue
	}
	return parsed
}

func cloneToolDescriptor(descriptor protocol.ToolDescriptor) (protocol.ToolDescriptor, error) {
	encoded, err := json.Marshal(descriptor)
	if err != nil {
		return protocol.ToolDescriptor{}, fmt.Errorf("复制工具 %s 契约: %w", descriptor.Name, err)
	}
	var cloned protocol.ToolDescriptor
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		return protocol.ToolDescriptor{}, fmt.Errorf("复制工具 %s 契约: %w", descriptor.Name, err)
	}
	return cloned, nil
}

func mergeSchemaMaps(path string, left, right map[string]any) (map[string]any, error) {
	if left == nil || right == nil {
		if left == nil && right == nil {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: %s schema presence differs", ErrIncompatibleToolContract, path)
	}
	leftValue, err := normalizeJSONValue(left)
	if err != nil {
		return nil, err
	}
	rightValue, err := normalizeJSONValue(right)
	if err != nil {
		return nil, err
	}
	return mergeSchemaObjects(path, leftValue.(map[string]any), rightValue.(map[string]any))
}

func mergeSchemaObjects(path string, left, right map[string]any) (map[string]any, error) {
	leftRequired, err := schemaRequiredSet(left)
	if err != nil {
		return nil, fmt.Errorf("%w: %s.required: %v", ErrIncompatibleToolContract, path, err)
	}
	rightRequired, err := schemaRequiredSet(right)
	if err != nil {
		return nil, fmt.Errorf("%w: %s.required: %v", ErrIncompatibleToolContract, path, err)
	}
	if !reflect.DeepEqual(leftRequired, rightRequired) {
		return nil, fmt.Errorf("%w: %s.required differs", ErrIncompatibleToolContract, path)
	}

	result := make(map[string]any, len(left)+len(right))
	keys := unionMapKeys(left, right)
	for _, key := range keys {
		leftValue, leftOK := left[key]
		rightValue, rightOK := right[key]
		switch key {
		case "title", "description":
			if leftOK {
				result[key] = leftValue
			} else if rightOK {
				result[key] = rightValue
			}
		case "required":
			if len(leftRequired) > 0 {
				required := make([]string, 0, len(leftRequired))
				for item := range leftRequired {
					required = append(required, item)
				}
				sort.Strings(required)
				result[key] = required
			} else if leftOK {
				result[key] = leftValue
			} else if rightOK {
				result[key] = rightValue
			}
		case "properties":
			properties, err := mergeSchemaProperties(path+".properties", leftValue, leftOK, rightValue, rightOK, leftRequired, rightRequired)
			if err != nil {
				return nil, err
			}
			if properties != nil {
				result[key] = properties
			}
		default:
			if !leftOK || !rightOK || !semanticSchemaValuesEqual(leftValue, rightValue, key) {
				return nil, fmt.Errorf("%w: %s.%s differs", ErrIncompatibleToolContract, path, key)
			}
			result[key] = leftValue
		}
	}
	return result, nil
}

func mergeSchemaProperties(path string, leftValue any, leftOK bool, rightValue any, rightOK bool, leftRequired, rightRequired map[string]struct{}) (map[string]any, error) {
	left, err := schemaProperties(leftValue, leftOK)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrIncompatibleToolContract, path, err)
	}
	right, err := schemaProperties(rightValue, rightOK)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrIncompatibleToolContract, path, err)
	}
	if !leftOK && !rightOK {
		return nil, nil
	}

	result := make(map[string]any, len(left)+len(right))
	for _, name := range unionMapKeys(left, right) {
		leftSchema, inLeft := left[name]
		rightSchema, inRight := right[name]
		switch {
		case inLeft && inRight:
			leftMap, leftIsMap := leftSchema.(map[string]any)
			rightMap, rightIsMap := rightSchema.(map[string]any)
			if leftIsMap && rightIsMap {
				merged, err := mergeSchemaObjects(path+"."+name, leftMap, rightMap)
				if err != nil {
					return nil, err
				}
				result[name] = merged
				continue
			}
			if !semanticSchemaValuesEqual(leftSchema, rightSchema, name) {
				return nil, fmt.Errorf("%w: %s.%s differs", ErrIncompatibleToolContract, path, name)
			}
			result[name] = leftSchema
		case inLeft:
			if _, required := leftRequired[name]; required {
				return nil, fmt.Errorf("%w: %s.%s only exists on one provider but is required", ErrIncompatibleToolContract, path, name)
			}
			result[name] = leftSchema
		case inRight:
			if _, required := rightRequired[name]; required {
				return nil, fmt.Errorf("%w: %s.%s only exists on one provider but is required", ErrIncompatibleToolContract, path, name)
			}
			result[name] = rightSchema
		}
	}
	return result, nil
}

func schemaProperties(value any, exists bool) (map[string]any, error) {
	if !exists {
		return map[string]any{}, nil
	}
	properties, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("properties 不是 JSON object")
	}
	return properties, nil
}

func schemaRequiredSet(schema map[string]any) (map[string]struct{}, error) {
	set := make(map[string]struct{})
	value, ok := schema["required"]
	if !ok {
		return set, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, errors.New("required 不是数组")
	}
	for _, item := range items {
		name, ok := item.(string)
		if !ok {
			return nil, errors.New("required 包含非字符串值")
		}
		set[name] = struct{}{}
	}
	return set, nil
}

func semanticSchemaValuesEqual(left, right any, parentKey string) bool {
	leftNormalized := semanticSchemaKeywordValue(parentKey, left)
	rightNormalized := semanticSchemaKeywordValue(parentKey, right)
	return reflect.DeepEqual(leftNormalized, rightNormalized)
}

func jsonValuesEqual(left, right any) bool {
	leftNormalized, leftErr := normalizeJSONValue(left)
	rightNormalized, rightErr := normalizeJSONValue(right)
	return leftErr == nil && rightErr == nil && reflect.DeepEqual(leftNormalized, rightNormalized)
}

func unionMapKeys(left, right map[string]any) []string {
	keys := make(map[string]struct{}, len(left)+len(right))
	for key := range left {
		keys[key] = struct{}{}
	}
	for key := range right {
		keys[key] = struct{}{}
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	return ordered
}

func normalizeSemanticHashes(hashes []string) []string {
	unique := make(map[string]struct{}, len(hashes))
	for _, hash := range hashes {
		hash = strings.TrimSpace(hash)
		if hash != "" {
			unique[hash] = struct{}{}
		}
	}
	normalized := make([]string, 0, len(unique))
	for hash := range unique {
		normalized = append(normalized, hash)
	}
	sort.Strings(normalized)
	return normalized
}

// ContainsHash 判断契约哈希是否在已接受的兼容集合内。
// 集合由 normalizeSemanticHashes 排序去重，因此可二分查找。
func ContainsHash(hashes []string, target string) bool {
	index := sort.SearchStrings(hashes, target)
	return index < len(hashes) && hashes[index] == target
}

func findToolDescriptor(descriptors []protocol.ToolDescriptor, name string) (protocol.ToolDescriptor, bool) {
	for _, descriptor := range descriptors {
		if descriptor.Name == name {
			return descriptor, true
		}
	}
	return protocol.ToolDescriptor{}, false
}

func containsCapability(capabilities []string, name string) bool {
	for _, capability := range capabilities {
		if capability == name {
			return true
		}
	}
	return false
}
