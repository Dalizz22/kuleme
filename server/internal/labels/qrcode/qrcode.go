// Package qrcode 生成标签二维码矩阵，底层用开源库 rsc.io/qr。
//
// 之前自己实现过一版 ISO/IEC 18004 子集（约 470 行），先后暴露出数据码字
// 填充、码眼结构、格式信息位序三处真 bug——二维码是标签的核心，扫不出的码
// 等于整个打印功能报废，这种轮子不该自己造。现在的职责只剩三层薄封装：
//
//   - 保持项目内的 Code 接口（Size / Dark），PDF 渲染按模块画矩形；
//   - EncodeAny 的错误映射保住 ErrTooLong 的语义（调用方据此提示而不是
//     静默截断）；
//   - 输入只接受可打印 ASCII（payload 由 kod.QRPayload 产生，天然满足）。
//
// 纠错等级固定 M；rsc.io/qr 自动选最小可容纳版本。
package qrcode

import (
	"errors"
	"fmt"

	"rsc.io/qr"
)

// ErrTooLong 表示内容超出实现的容量上限。
var ErrTooLong = errors.New("二维码内容过长")

// ErrNotASCII 表示内容含非 ASCII 字节，超出本实现支持范围。
var ErrNotASCII = errors.New("二维码内容必须是 ASCII")

// ErrEmpty 表示内容为空。
var ErrEmpty = errors.New("二维码内容为空")

// Code 是一个已生成的二维码矩阵。Size 是边长（含功能图形，不含静区），
// Dark 的 (row, col) 语义与项目内渲染约定一致。
type Code struct {
	inner *qr.Code
	Size  int
}

// Dark 报告 (row, col) 是否为深色模块，越界返回 false。
func (c Code) Dark(row, col int) bool {
	if row < 0 || col < 0 || row >= c.Size || col >= c.Size {
		return false
	}
	return c.inner.Black(col, row)
}

// EncodeAny 在可容纳的最小版本中生成二维码矩阵。
func EncodeAny(content string) (Code, error) {
	if content == "" {
		return Code{}, ErrEmpty
	}
	if !isASCII(content) {
		return Code{}, fmt.Errorf("%w: %q", ErrNotASCII, content)
	}
	inner, err := qr.Encode(content, qr.M)
	if err != nil {
		return Code{}, fmt.Errorf("%w: %v", ErrTooLong, err)
	}
	return Code{inner: inner, Size: inner.Size}, nil
}

// isASCII 报告内容是否全部由可打印 ASCII 组成。
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7E {
			return false
		}
	}
	return len(s) > 0
}
