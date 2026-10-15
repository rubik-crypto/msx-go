package logistic

import (
	"math"
	"math/rand"
	"testing"
)

// 线性可分的合成数据：y=1 当且仅当 x0+x1 > 1。
//
// 落在分界线附近的点会被丢掉（间隔是 0.08）。留着它们的话，损失只有在权重推到
// 无穷大时才能降到 0，梯度下降几百轮根本走不到，测试就没法有一条干净的判定标准。
func separableData(n int, seed int64) ([][]float64, []float64) {
	const margin = 0.08
	rng := rand.New(rand.NewSource(seed))
	x := make([][]float64, 0, n)
	y := make([]float64, 0, n)
	for len(x) < n {
		a, b := rng.Float64(), rng.Float64()
		if math.Abs(a+b-1) < margin {
			continue
		}
		label := 0.0
		if a+b > 1 {
			label = 1
		}
		x = append(x, []float64{a, b})
		y = append(y, label)
	}
	return x, y
}

func TestTrainSeparableData(t *testing.T) {
	x, y := separableData(2000, 1)
	m, err := Train(x, y, Options{Epochs: 300, LearningRate: 1.0, ShrinkOnRise: true})
	if err != nil {
		t.Fatalf("训练失败：%v", err)
	}
	if acc := m.Accuracy(x, y); acc < 0.97 {
		t.Fatalf("线性可分数据上准确率只有 %.4f，应该接近 1", acc)
	}
	if auc := m.AUC(x, y); auc < 0.99 {
		t.Fatalf("线性可分数据上 AUC 只有 %.4f，应该接近 1", auc)
	}
	// 两个特征的权重应该都是正的（都指向正类）。
	for i, w := range m.Weights {
		if w <= 0 {
			t.Fatalf("第 %d 个特征的权重是 %.4f，在这份数据上应该是正的", i, w)
		}
	}
	// 损失必须是一路降到接近 0，而不是震荡。
	if m.Loss[len(m.Loss)-1] > 0.15 {
		t.Fatalf("最终损失 %.4f 偏高，没收敛", m.Loss[len(m.Loss)-1])
	}
	if m.Loss[len(m.Loss)-1] > m.Loss[0] {
		t.Fatalf("损失没有下降：首轮 %.4f，末轮 %.4f", m.Loss[0], m.Loss[len(m.Loss)-1])
	}
}

// 标签完全随机时，模型不可能真的学到东西：
// 损失应该停在 ln2 附近，AUC 应该接近 0.5。
func TestTrainRandomLabels(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	n := 3000
	x := make([][]float64, n)
	y := make([]float64, n)
	for i := 0; i < n; i++ {
		x[i] = []float64{rng.Float64(), rng.Float64(), rng.Float64()}
		if rng.Float64() < 0.5 {
			y[i] = 1
		}
	}
	m, err := Train(x, y, Options{Epochs: 200, LearningRate: 1.0, L2: 0.001, ShrinkOnRise: true})
	if err != nil {
		t.Fatalf("训练失败：%v", err)
	}
	if got := m.LogLoss(x, y); got < 0.68 {
		t.Fatalf("随机标签上的损失 %.4f 明显低于 ln2(0.693)，八成是过拟合了", got)
	}
	if auc := m.AUC(x, y); math.Abs(auc-0.5) > 0.06 {
		t.Fatalf("随机标签上的 AUC 是 %.4f，应该接近 0.5", auc)
	}
	// 基准线也不能比模型好太多。
	if base := BaseLogLoss(y); math.Abs(base-math.Log(2)) > 0.01 {
		t.Fatalf("正负各半时 BaseLogLoss 应为 ln2，实际 %.4f", base)
	}
}

func TestBaseLogLoss(t *testing.T) {
	// 全部是正类：常数模型可以完美预测，损失为 0。
	if got := BaseLogLoss([]float64{1, 1, 1}); got != 0 {
		t.Fatalf("全正类时 BaseLogLoss 应为 0，实际 %v", got)
	}
	// 三正一负：p=0.75，损失 = -(0.75*ln0.75 + 0.25*ln0.25)
	want := -(0.75*math.Log(0.75) + 0.25*math.Log(0.25))
	if got := BaseLogLoss([]float64{1, 1, 1, 0}); math.Abs(got-want) > 1e-12 {
		t.Fatalf("BaseLogLoss 应为 %v，实际 %v", want, got)
	}
	if got := BaseLogLoss(nil); got != 0 {
		t.Fatalf("空输入应为 0，实际 %v", got)
	}
}

func TestMetricsOnHandBuiltModel(t *testing.T) {
	// 手工造一个模型：只用一个特征，权重大于 0，那么分数越大概率越高。
	m := &Model{Weights: []float64{2}, Bias: -1}
	x := [][]float64{{0}, {0.4}, {0.6}, {1}}
	y := []float64{0, 0, 1, 1}

	if m.Predict(x[0]) != false || m.Predict(x[3]) != true {
		t.Fatalf("阈值判断不对")
	}
	if acc := m.Accuracy(x, y); acc != 1 {
		t.Fatalf("准确率应为 1，实际 %v", acc)
	}
	if auc := m.AUC(x, y); auc != 1 {
		t.Fatalf("完全分开时 AUC 应为 1，实际 %v", auc)
	}
	// 概率必须落在开区间里，两端不能贴到 0 或 1。
	for _, row := range x {
		p := m.Probability(row)
		if p <= 0 || p >= 1 {
			t.Fatalf("概率 %v 落在开区间外", p)
		}
	}
}

// 分数并列时，AUC 应该按平均秩算，结果正好是 0.5。
func TestAUCHandlesTies(t *testing.T) {
	m := &Model{Weights: []float64{0}, Bias: 0} // 所有样本分数相同
	x := [][]float64{{1}, {2}, {3}, {4}}
	y := []float64{0, 0, 1, 1}
	if got := m.AUC(x, y); math.Abs(got-0.5) > 1e-12 {
		t.Fatalf("全部并列时 AUC 应为 0.5，实际 %v", got)
	}
}

func TestAUCUndefinedClasses(t *testing.T) {
	m := &Model{Weights: []float64{1}, Bias: 0}
	x := [][]float64{{1}, {2}}
	if got := m.AUC(x, []float64{1, 1}); got != 0.5 {
		t.Fatalf("只有一类样本时 AUC 应为 0.5，实际 %v", got)
	}
}

func TestBrierScore(t *testing.T) {
	// 概率和标签完全一致时 Brier 为 0（这里用极端的偏置把概率压到接近 0/1）。
	m := &Model{Weights: []float64{0}, Bias: -100}
	x := [][]float64{{1}, {2}}
	if got := m.BrierScore(x, []float64{0, 0}); got > 1e-9 {
		t.Fatalf("预测和标签一致时 Brier 应接近 0，实际 %v", got)
	}
	if got := m.BrierScore(x, []float64{1, 1}); got < 0.99 {
		t.Fatalf("预测完全相反时 Brier 应接近 1，实际 %v", got)
	}
}

func TestTrainRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		x    [][]float64
		y    []float64
		opt  Options
	}{
		{"样本和标签数不一致", [][]float64{{1}}, nil, Options{Epochs: 1, LearningRate: 1}},
		{"没有样本", nil, nil, Options{Epochs: 1, LearningRate: 1}},
		{"Epochs 为 0", [][]float64{{1}}, []float64{1}, Options{LearningRate: 1}},
		{"学习率为 0", [][]float64{{1}}, []float64{1}, Options{Epochs: 1}},
		{"特征维度为 0", [][]float64{{}}, []float64{1}, Options{Epochs: 1, LearningRate: 1}},
		{"特征长度不一致", [][]float64{{1}, {1, 2}}, []float64{0, 1}, Options{Epochs: 1, LearningRate: 1}},
	}
	for _, c := range cases {
		if _, err := Train(c.x, c.y, c.opt); err == nil {
			t.Fatalf("%s：应该返回错误", c.name)
		}
	}
}

// 同样输入必须训出同样结果，否则没法复现问题、也没法把模型存下来。
func TestTrainIsDeterministic(t *testing.T) {
	x, y := separableData(500, 3)
	a, err := Train(x, y, Options{Epochs: 50, LearningRate: 1.0})
	if err != nil {
		t.Fatalf("训练失败：%v", err)
	}
	b, err := Train(x, y, Options{Epochs: 50, LearningRate: 1.0})
	if err != nil {
		t.Fatalf("训练失败：%v", err)
	}
	if a.Bias != b.Bias {
		t.Fatalf("两次训练的偏置不同：%v vs %v", a.Bias, b.Bias)
	}
	for i := range a.Weights {
		if a.Weights[i] != b.Weights[i] {
			t.Fatalf("两次训练的第 %d 个权重不同", i)
		}
	}
}

func TestOnEpochCallback(t *testing.T) {
	x, y := separableData(200, 5)
	seen := 0
	lastEpoch := -1
	_, err := Train(x, y, Options{
		Epochs:       12,
		LearningRate: 1.0,
		OnEpoch: func(e int, loss float64) {
			seen++
			if e != lastEpoch+1 {
				t.Errorf("轮次不连续：上一轮 %d，这一轮 %d", lastEpoch, e)
			}
			lastEpoch = e
			if math.IsNaN(loss) || math.IsInf(loss, 0) {
				t.Errorf("第 %d 轮损失是 %v", e, loss)
			}
		},
	})
	if err != nil {
		t.Fatalf("训练失败：%v", err)
	}
	if seen != 12 {
		t.Fatalf("回调应该被调用 12 次，实际 %d 次", seen)
	}
}

// 学习率给得过大时，靠 ShrinkOnRise 应该能自己收敛下来。
func TestShrinkOnRiseRecoversFromLargeStep(t *testing.T) {
	x, y := separableData(1000, 9)
	m, err := Train(x, y, Options{Epochs: 200, LearningRate: 500, ShrinkOnRise: true})
	if err != nil {
		t.Fatalf("训练失败：%v", err)
	}
	last := m.Loss[len(m.Loss)-1]
	if math.IsNaN(last) || math.IsInf(last, 0) {
		t.Fatalf("最终损失是 %v，训练发散了", last)
	}
	if last > m.Loss[0] {
		t.Fatalf("损失没有降下来：首轮 %.4f，末轮 %.4f", m.Loss[0], last)
	}
}
