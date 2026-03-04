package main

// nextValidN2VCPUs rounds n up to the next valid N2 machine type vCPU count.
// Valid N2 vCPU counts are: 2, 4, 8, 16, 32, 48, 64, 80, 96, 128.
func nextValidN2VCPUs(n int) int {
	valid := []int{2, 4, 8, 16, 32, 48, 64, 80, 96, 128}
	for _, v := range valid {
		if v >= n {
			return v
		}
	}
	return valid[len(valid)-1]
}
