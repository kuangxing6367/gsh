package api

import (
	"strings"
	"testing"
)

// resetScale 把校准系数恢复到 1，避免用例之间互相影响。
func resetScale(t *testing.T) {
	t.Helper()
	tokenScale.Store(1.0)
	t.Cleanup(func() { tokenScale.Store(1.0) })
}

func currentScale() float64 {
	f, _ := tokenScale.Load().(float64)
	return f
}

// TestEstimateCJKNotUnderestimated 中文不能再按 2/3 估。
// 低估的直接后果是压缩判定偏晚：请求带着超窗口的历史发出去，
// provider 直接回 400，整轮对话失败。
func TestEstimateCJKNotUnderestimated(t *testing.T) {
	resetScale(t)
	const n = 1000
	if got := estimateText(strings.Repeat("中", n)); got < n {
		t.Fatalf("1000 个汉字估值 %d，低于下限 1000", got)
	}
	// ASCII 仍按 4 字符 1 token。
	if got := estimateText(strings.Repeat("a", 400)); got > 130 {
		t.Fatalf("400 个 ASCII 估成 %d，明显偏高", got)
	}
}

// TestCalibrateMovesTowardActual 真实用量远高于估算时，系数应往上走。
func TestCalibrateMovesTowardActual(t *testing.T) {
	resetScale(t)
	// 真实调用里传进来的是「已缩放」的估算值，这里必须同样模拟，
	// 否则等于对同一个原始值反复校准，比例会被放大。
	est := func() int { return scaled(1000) }
	CalibrateTokens(est(), 2000)
	if got := currentScale(); got <= 1.0 || got >= 2.0 {
		t.Fatalf("一次校准后系数应在 1 和 2 之间，实际 %.3f", got)
	}
	// 再多校准几轮，应逼近 2。
	for i := 0; i < 30; i++ {
		CalibrateTokens(est(), 2000)
	}
	if got := currentScale(); got < 1.9 || got > 2.05 {
		t.Fatalf("多轮校准后应逼近 2.0，实际 %.3f", got)
	}
}

// TestCalibrateConvergesWithoutDrift 校准过的估算值再拿去校准，
// 不能因为「估算已经乘过系数」而每轮再乘一遍，越走越偏。
func TestCalibrateConvergesWithoutDrift(t *testing.T) {
	resetScale(t)
	msgs := []Message{{Role: RoleUser, Content: strings.Repeat("中", 1000)}}
	// 模拟真实用量恒等于原始估算的 1.5 倍。
	rawEstimate := 1500
	for i := 0; i < 40; i++ {
		est := EstimateTokensOf(msgs)
		if est == 0 {
			t.Fatal("估算值不该为 0")
		}
		// 真实值固定为「原始估算 × 1.5」，与当前系数无关。
		CalibrateTokens(est, rawEstimate*3/2)
	}
	// 收敛后，缩放后的估算应接近真实值的量级，而不是指数级膨胀。
	got := EstimateTokensOf(msgs)
	if got > rawEstimate*4 {
		t.Fatalf("估算值发散：%d（真实约 %d）", got, rawEstimate*3/2)
	}
	if got < rawEstimate {
		t.Fatalf("估算值收敛过头：%d（真实约 %d）", got, rawEstimate*3/2)
	}
}

// TestCalibrateIgnoresTinySamples 样本太小时比例没有统计意义，不该动系数。
func TestCalibrateIgnoresTinySamples(t *testing.T) {
	resetScale(t)
	CalibrateTokens(20, 2000)
	if got := currentScale(); got != 1.0 {
		t.Fatalf("小样本不该影响系数，实际 %.3f", got)
	}
}

// TestCalibrateClampsOutliers 单轮离谱的用量不能把系数带到 100 倍。
func TestCalibrateClampsOutliers(t *testing.T) {
	resetScale(t)
	for i := 0; i < 50; i++ {
		CalibrateTokens(1000, 1000000)
	}
	if got := currentScale(); got > 3.05 {
		t.Fatalf("系数应被夹在上限 3，实际 %.3f", got)
	}
}
