package receipt

import "math"

// pValue is the two-sided p-value of Student's t statistic with df degrees
// of freedom: I_x(df/2, 1/2) with x = df/(df+t^2). It is NaN when df is
// not positive or t is NaN, so the caller can leave it out.
func pValue(t, df float64) float64 {
	switch {
	case !(df > 0) || math.IsNaN(t):
		return math.NaN()
	case math.IsInf(t, 0):
		return 0
	}
	return regIncBeta(df/2, 0.5, df/(df+t*t))
}

// regIncBeta is the regularized incomplete beta function I_x(a, b), by the
// continued fraction on whichever side converges (Numerical Recipes 6.4).
func regIncBeta(a, b, x float64) float64 {
	switch {
	case x <= 0:
		return 0
	case x >= 1:
		return 1
	}
	la, _ := math.Lgamma(a)
	lb, _ := math.Lgamma(b)
	lab, _ := math.Lgamma(a + b)
	front := math.Exp(lab - la - lb + a*math.Log(x) + b*math.Log1p(-x))
	if x < (a+1)/(a+b+2) {
		return front * betaFraction(a, b, x) / a
	}
	return 1 - front*betaFraction(b, a, 1-x)/b
}

// betaFraction evaluates the incomplete beta continued fraction with the
// modified Lentz method.
func betaFraction(a, b, x float64) float64 {
	const (
		maxIter = 10000
		eps     = 1e-15
		tiny    = 1e-300
	)
	qab, qap, qam := a+b, a+1, a-1
	c, d := 1.0, 1-qab*x/qap
	if math.Abs(d) < tiny {
		d = tiny
	}
	d = 1 / d
	h := d
	for m := 1; m <= maxIter; m++ {
		fm := float64(m)
		m2 := 2 * fm
		aa := fm * (b - fm) * x / ((qam + m2) * (a + m2))
		d, c = lentz(d, c, aa, tiny)
		h *= d * c
		aa = -(a + fm) * (qab + fm) * x / ((a + m2) * (qap + m2))
		d, c = lentz(d, c, aa, tiny)
		del := d * c
		h *= del
		if math.Abs(del-1) < eps {
			break
		}
	}
	return h
}

// lentz is one step of the modified Lentz recurrence.
func lentz(d, c, aa, tiny float64) (float64, float64) {
	d = 1 + aa*d
	if math.Abs(d) < tiny {
		d = tiny
	}
	c = 1 + aa/c
	if math.Abs(c) < tiny {
		c = tiny
	}
	return 1 / d, c
}
