package roundsum

import (
	"math"
	"testing"
)

func TestRound(t *testing.T) {
	cases := []struct{ in float64; digits int; want float64 }{
		{1.005, 2, 1.01},
		{1.004, 2, 1.0},
		{2.675, 2, 2.68},
		{123.456, 1, 123.5},
		{-1.005, 2, -1.01},
		{7.000000000000001, 2, 7},
		{3, 0, 3},
	}
	for _, c := range cases {
		if got := Round(c.in, c.digits); got != c.want {
			t.Errorf("Round(%v, %d) = %v，期望 %v", c.in, c.digits, got, c.want)
		}
	}
}

// 分项先圆再加：页面上按两位小数渲染，相加必须正好等于合计。
func TestPartsAddUp(t *testing.T) {
	parts := []float64{0.005, 0.005, 0.005, 0.005}
	rounded, total := Parts(parts, 2)
	sum := 0.0
	for _, v := range rounded {
		// 模拟页面上两位小数的渲染
		sum += math.Round(v*100) / 100
	}
	if math.Abs(sum-total) > 1e-9 {
		t.Fatalf("分项之和 %v != 合计 %v（分项 %v）", sum, total, rounded)
	}
	// 反例：原始值相加再圆一次，和上面这套是不一样的（0.02 vs 0.02？这里 4×0.005=0.02，两边都是 0.02）
	// 用更极端的一组证明差别客观存在：
	odd := []float64{0.004, 0.004, 0.004}
	rounded2, total2 := Parts(odd, 2)
	if rounded2[0] != 0 || rounded2[1] != 0 || rounded2[2] != 0 || total2 != 0 {
		t.Fatalf("先圆后加应当全是 0，实际 %v / %v", rounded2, total2)
	}
	if Round(0.004+0.004+0.004, 2) == 0 {
		t.Fatalf("原始值相加再圆一次本该是 0.01（这就是对不上的来源）")
	}
}

func TestPartsEmpty(t *testing.T) {
	r, total := Parts(nil, 2)
	if len(r) != 0 || total != 0 {
		t.Fatalf("空输入应当返回空切片和 0，实际 %v / %v", r, total)
	}
}

func TestSplitSumsToTotal(t *testing.T) {
	cases := []struct {
		total   float64
		weights []float64
		digits  int
	}{
		{100, []float64{1, 1, 1}, 0},
		{100, []float64{1, 2, 3}, 0},
		{10, []float64{1, 1, 1, 1, 1, 1, 1}, 0},
		{104.37, []float64{33.333, 1, 0.001}, 2},
		{0.03, []float64{1, 1, 1}, 2},
		{7, []float64{0, 0, 0}, 1},
		{-9, []float64{1, 1, 1, 1}, 0},
		{1234.567, []float64{5, 0, 5}, 2},
	}
	for _, c := range cases {
		got := Split(c.total, c.weights, c.digits)
		if len(got) != len(c.weights) {
			t.Fatalf("份数不对：%v", got)
		}
		sum := 0.0
		for _, v := range got {
			sum += v
		}
		if math.Abs(sum-Round(c.total, c.digits)) > 1e-9 {
			t.Errorf("Split(%v, %v, %d) = %v，相加 %v ≠ %v", c.total, c.weights, c.digits, got, sum, Round(c.total, c.digits))
		}
	}
}

func TestPercentSumsTo100(t *testing.T) {
	for _, w := range [][]float64{
		{1, 1, 1},
		{1, 2, 3, 4},
		{5, 0, 5},
		{0.5, 0.25, 0.25},
		{1, 0, 0},
		{999, 1},
	} {
		got := Percent(w)
		sum := 0
		for _, v := range got {
			sum += v
		}
		if sum != 100 {
			t.Errorf("Percent(%v) = %v，合计 %d ≠ 100", w, got, sum)
		}
		for _, v := range got {
			if v < 0 {
				t.Errorf("Percent(%v) = %v 里出现了负数", w, got)
			}
		}
	}
}

func TestSplitWeightZeroFallsBackToEqual(t *testing.T) {
	got := Split(3, []float64{0, 0, 0}, 0)
	for _, v := range got {
		if v != 1 {
			t.Fatalf("权重全 0 时应当平均分，实际 %v", got)
		}
	}
}

func TestSplitEmpty(t *testing.T) {
	if got := Split(10, nil, 2); got != nil {
		t.Fatalf("没有权重时应当返回 nil，实际 %v", got)
	}
}
