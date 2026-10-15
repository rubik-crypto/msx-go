// Package logistic 提供一个只依赖标准库的批量逻辑回归实现：
// 用全批量梯度下降加 L2 正则训练二分类模型，并给出损失、准确率、Brier 分数和 AUC 这些常用评估指标。
//
// 它面向的是「特征维度不高、样本量几千到几十万、要在服务里现场训练」的场景，
// 不追求大规模稀疏数据上的性能，追求的是依赖为零、行为可解释、训练过程可观察。
//
// 典型用法：
//
//	opt := logistic.Options{Epochs: 200, LearningRate: 1.0, L2: 0.001}
//	m, err := logistic.Train(x, y, opt)
//	if err != nil {
//		return err
//	}
//	p := m.Probability(x[0])   // 这一条样本属于正类的概率
//
// 训练前请自己把特征缩放到相近的量级（比如都落在 0~1），
// 否则梯度下降会在量级最大的那个特征上震荡。
package logistic

import (
	"errors"
	"math"
	"sort"
)

// Options 是训练参数。零值不可用，至少要给 Epochs 和 LearningRate。
type Options struct {
	// Epochs 是把全部样本过多少遍。几百遍通常就够，损失曲线会告诉你什么时候该停。
	Epochs int
	// LearningRate 是初始学习率。特征缩放到 0~1 附近时，1.0 是个好起点。
	LearningRate float64
	// L2 是 L2 正则强度。样本量小的时候给 1e-3 左右能压住过拟合；0 表示不正则。
	L2 float64
	// ShrinkOnRise 为 true 时，某一轮的损失比上一轮高就把学习率减半。
	// 特征量级没调好、或者学习率给大了导致来回震荡时，打开它能自己救回来。
	ShrinkOnRise bool
	// OnEpoch 每一轮结束后回调一次（轮次从 0 开始，损失是这一轮的平均交叉熵）。
	// 想画损失曲线或者打日志时用它，不需要就把损失从 Model.Loss 里取。
	OnEpoch func(epoch int, loss float64)
}

// Model 是训练好的模型。Weights 和 Bias 可以直接序列化存下来，下次不用重训。
type Model struct {
	Weights []float64
	Bias    float64
	// Loss 是每一轮的平均交叉熵，用来判断收敛情况。
	Loss []float64
}

// Train 用全批量梯度下降训练一个逻辑回归。
//
// x 的每一行是一条样本的特征，y 是它对应的标签，取值 0 或 1。
// 返回的 Model 即使样本为空也不会是 nil（权重全是 0），但样本为空时会返回错误，
// 免得调用方拿到一个「能跑但毫无意义」的模型。
func Train(x [][]float64, y []float64, opt Options) (*Model, error) {
	if len(x) != len(y) {
		return nil, errors.New("logistic: 样本数和标签数不一致")
	}
	if len(x) == 0 {
		return nil, errors.New("logistic: 没有样本")
	}
	if opt.Epochs <= 0 {
		return nil, errors.New("logistic: Epochs 必须大于 0")
	}
	if opt.LearningRate <= 0 {
		return nil, errors.New("logistic: LearningRate 必须大于 0")
	}
	dim := len(x[0])
	if dim == 0 {
		return nil, errors.New("logistic: 特征维度是 0")
	}
	for i := range x {
		if len(x[i]) != dim {
			return nil, errors.New("logistic: 每条样本的特征个数必须一样")
		}
	}

	m := &Model{
		Weights: make([]float64, dim),
		Loss:    make([]float64, 0, opt.Epochs),
	}
	grad := make([]float64, dim)
	n := float64(len(x))
	lr := opt.LearningRate
	prev := math.Inf(1)

	for e := 0; e < opt.Epochs; e++ {
		for i := range grad {
			grad[i] = 0
		}
		biasGrad := 0.0
		loss := 0.0

		for i := range x {
			p := m.Probability(x[i])
			d := p - y[i]
			for j := 0; j < dim; j++ {
				grad[j] += d * x[i][j]
			}
			biasGrad += d
			loss += crossEntropy(y[i], p)
		}

		// L2 只压权重，不压偏置 —— 压偏置会让模型没法拟合正负样本不平衡的数据。
		for j := 0; j < dim; j++ {
			m.Weights[j] -= lr * (grad[j]/n + opt.L2*m.Weights[j])
		}
		m.Bias -= lr * biasGrad / n

		loss /= n
		m.Loss = append(m.Loss, loss)
		if opt.OnEpoch != nil {
			opt.OnEpoch(e, loss)
		}
		// 只在「真的涨了」的时候缩学习率。
		// 用严格大于会出事：快到极小值点时损失会在浮点噪声里上下抖百分之几的亿分之一，
		// 每一轮都当成上升去减半，学习率很快就会掉到 1e-9，训练在离最优很远的地方停住。
		if opt.ShrinkOnRise && loss > prev*(1+1e-6) {
			lr *= 0.5
			const minLr = 1e-12
			if lr < minLr {
				lr = minLr
			}
		}
		prev = loss
	}
	return m, nil
}

// Logit 是线性部分的取值（还没过 sigmoid）。
func (m *Model) Logit(x []float64) float64 {
	z := m.Bias
	for j := 0; j < len(m.Weights) && j < len(x); j++ {
		z += m.Weights[j] * x[j]
	}
	return z
}

// Probability 返回这条样本属于正类的概率，落在 (0, 1) 开区间内。
func (m *Model) Probability(x []float64) float64 {
	return sigmoid(m.Logit(x))
}

// Predict 按 0.5 的概率阈值给出类别。
func (m *Model) Predict(x []float64) bool {
	return m.Logit(x) > 0
}

// crossEntropy 是对数损失。概率被夹在 (0,1) 开区间里，所以不会返回 ±Inf。
func crossEntropy(y, p float64) float64 {
	if y > 0.5 {
		return -math.Log(p)
	}
	return -math.Log(1 - p)
}

// sigmoid 把任意实数压到 (0,1)。两端各留一点余量，免得后面取对数时炸掉。
func sigmoid(z float64) float64 {
	p := 1 / (1 + math.Exp(-z))
	const eps = 1e-12
	if p < eps {
		p = eps
	}
	if p > 1-eps {
		p = 1 - eps
	}
	return p
}

// LogLoss 返回模型在给定样本上的平均对数损失。越小越好。
// 交叉熵对概率很敏感：把「确实是正类」的样本只给 0.1 的概率，损失会很大，
// 所以它比准确率更能反映一个概率模型的成色。
func (m *Model) LogLoss(x [][]float64, y []float64) float64 {
	n := minLen(x, y)
	if n == 0 {
		return 0
	}
	sum := 0.0
	for i := 0; i < n; i++ {
		sum += crossEntropy(y[i], m.Probability(x[i]))
	}
	return sum / float64(n)
}

// BaseLogLoss 返回「不看任何特征、每一条都输出同一个概率」时的对数损失。
// 它是判断模型有没有真正学到东西的基准线：模型自己的 LogLoss 要是没比这个低，
// 那这些特征就白给了。
func BaseLogLoss(y []float64) float64 {
	if len(y) == 0 {
		return 0
	}
	pos := 0.0
	for _, v := range y {
		if v > 0.5 {
			pos++
		}
	}
	p := pos / float64(len(y))
	if p <= 0 || p >= 1 {
		return 0
	}
	return -(p*math.Log(p) + (1-p)*math.Log(1-p))
}

// Accuracy 是按 0.5 阈值判类别时的准确率。
func (m *Model) Accuracy(x [][]float64, y []float64) float64 {
	n := minLen(x, y)
	if n == 0 {
		return 0
	}
	hit := 0
	for i := 0; i < n; i++ {
		if m.Predict(x[i]) == (y[i] > 0.5) {
			hit++
		}
	}
	return float64(hit) / float64(n)
}

// BrierScore 返回概率预测的均方误差，越小越好，取值在 0~1 之间。
// 和 LogLoss 的区别是它对「差一点点」的预测更宽容，对「差很多」的惩罚更轻。
func (m *Model) BrierScore(x [][]float64, y []float64) float64 {
	n := minLen(x, y)
	if n == 0 {
		return 0
	}
	sum := 0.0
	for i := 0; i < n; i++ {
		d := m.Probability(x[i]) - y[i]
		sum += d * d
	}
	return sum / float64(n)
}

// AUC 是 ROC 曲线下的面积，衡量模型把正负样本分开的能力。
//
// 0.5 表示和随机猜没区别，1.0 表示完全分得开。
// 正样本或负样本一个都没有时返回 0.5（此时 AUC 没有意义）。
func (m *Model) AUC(x [][]float64, y []float64) float64 {
	n := minLen(x, y)
	if n == 0 {
		return 0.5
	}
	type pair struct {
		score float64
		pos   bool
	}
	ps := make([]pair, 0, n)
	posN, negN := 0, 0
	for i := 0; i < n; i++ {
		isPos := y[i] > 0.5
		if isPos {
			posN++
		} else {
			negN++
		}
		ps = append(ps, pair{score: m.Logit(x[i]), pos: isPos})
	}
	if posN == 0 || negN == 0 {
		return 0.5
	}

	sort.SliceStable(ps, func(a, b int) bool { return ps[a].score < ps[b].score })

	// 分数相同的样本要分到平均秩，否则 AUC 会被打分本身的并列关系带偏。
	rankSum := 0.0 // 正样本的秩之和，秩从 1 开始
	for i := 0; i < len(ps); {
		j := i
		for j+1 < len(ps) && ps[j+1].score == ps[i].score {
			j++
		}
		avgRank := float64(i+j)/2 + 1
		for k := i; k <= j; k++ {
			if ps[k].pos {
				rankSum += avgRank
			}
		}
		i = j + 1
	}
	return (rankSum - float64(posN)*float64(posN+1)/2) / (float64(posN) * float64(negN))
}

func minLen(x [][]float64, y []float64) int {
	n := len(x)
	if len(y) < n {
		n = len(y)
	}
	return n
}
