package coordconv

import "math"

// Point 是一组经纬度，单位为度。Lat 为纬度，Lng 为经度。
type Point struct {
	Lat float64
	Lng float64
}

const (
	semiMajorAxis = 6378245.0
	eccentricity  = 0.00669342162296594323
	baiduXPi      = math.Pi * 3000.0 / 180.0
)

// InOffsetArea 判断坐标是否落在 GCJ-02 偏移的近似范围内。
//
// 这里用的是各大地图 SDK 通用的经纬度矩形判断，它覆盖的范围略大于中国大陆，
// 东亚部分国家和地区也会落在这个矩形里。需要严格国界判断时应自行补充边界数据。
func InOffsetArea(p Point) bool {
	return !(p.Lng < 72.004 || p.Lng > 137.8347 || p.Lat < 0.8293 || p.Lat > 55.8271)
}

func transformLat(x, y float64) float64 {
	ret := -100.0 + 2.0*x + 3.0*y + 0.2*y*y + 0.1*x*y + 0.2*math.Sqrt(math.Abs(x))
	ret += (20.0*math.Sin(6.0*x*math.Pi) + 20.0*math.Sin(2.0*x*math.Pi)) * 2.0 / 3.0
	ret += (20.0*math.Sin(y*math.Pi) + 40.0*math.Sin(y/3.0*math.Pi)) * 2.0 / 3.0
	ret += (160.0*math.Sin(y/12.0*math.Pi) + 320.0*math.Sin(y*math.Pi/30.0)) * 2.0 / 3.0
	return ret
}

func transformLng(x, y float64) float64 {
	ret := 300.0 + x + 2.0*y + 0.1*x*x + 0.1*x*y + 0.1*math.Sqrt(math.Abs(x))
	ret += (20.0*math.Sin(6.0*x*math.Pi) + 20.0*math.Sin(2.0*x*math.Pi)) * 2.0 / 3.0
	ret += (20.0*math.Sin(x*math.Pi) + 40.0*math.Sin(x/3.0*math.Pi)) * 2.0 / 3.0
	ret += (150.0*math.Sin(x/12.0*math.Pi) + 300.0*math.Sin(x/30.0*math.Pi)) * 2.0 / 3.0
	return ret
}

// WGS84ToGCJ02 把 GPS 原始坐标（WGS-84）转成 GCJ-02，偏移范围之外原样返回。
func WGS84ToGCJ02(p Point) Point {
	if !InOffsetArea(p) {
		return p
	}
	dLat := transformLat(p.Lng-105.0, p.Lat-35.0)
	dLng := transformLng(p.Lng-105.0, p.Lat-35.0)
	radLat := p.Lat / 180.0 * math.Pi
	magic := math.Sin(radLat)
	magic = 1 - eccentricity*magic*magic
	sqrtMagic := math.Sqrt(magic)
	dLat = (dLat * 180.0) / ((semiMajorAxis * (1 - eccentricity)) / (magic * sqrtMagic) * math.Pi)
	dLng = (dLng * 180.0) / (semiMajorAxis / sqrtMagic * math.Cos(radLat) * math.Pi)
	return Point{Lat: p.Lat + dLat, Lng: p.Lng + dLng}
}

// GCJ02ToWGS84 用迭代逼近求 GCJ-02 的反变换，误差远小于一米；偏移范围之外原样返回。
func GCJ02ToWGS84(p Point) Point {
	if !InOffsetArea(p) {
		return p
	}
	out := p
	for i := 0; i < 4; i++ {
		g := WGS84ToGCJ02(out)
		out.Lat -= g.Lat - p.Lat
		out.Lng -= g.Lng - p.Lng
	}
	return out
}

// GCJ02ToBD09 把 GCJ-02 坐标转成百度使用的 BD-09。
func GCJ02ToBD09(p Point) Point {
	z := math.Sqrt(p.Lng*p.Lng+p.Lat*p.Lat) + 0.00002*math.Sin(p.Lat*baiduXPi)
	theta := math.Atan2(p.Lat, p.Lng) + 0.000003*math.Cos(p.Lng*baiduXPi)
	return Point{
		Lat: z*math.Sin(theta) + 0.006,
		Lng: z*math.Cos(theta) + 0.0065,
	}
}

// BD09ToGCJ02 把 BD-09 坐标转回 GCJ-02。
func BD09ToGCJ02(p Point) Point {
	x := p.Lng - 0.0065
	y := p.Lat - 0.006
	z := math.Sqrt(x*x+y*y) - 0.00002*math.Sin(y*baiduXPi)
	theta := math.Atan2(y, x) - 0.000003*math.Cos(x*baiduXPi)
	return Point{
		Lat: z * math.Sin(theta),
		Lng: z * math.Cos(theta),
	}
}

// WGS84ToBD09 把 WGS-84 坐标转成 BD-09。
func WGS84ToBD09(p Point) Point {
	return GCJ02ToBD09(WGS84ToGCJ02(p))
}

// BD09ToWGS84 把 BD-09 坐标转成 WGS-84。
func BD09ToWGS84(p Point) Point {
	return GCJ02ToWGS84(BD09ToGCJ02(p))
}
