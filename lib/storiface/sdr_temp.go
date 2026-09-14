package storiface

// SDRTempRoot holds independently owned attempt directories. The .tmp suffix
// excludes this root from normal sector discovery. It is distinct from legacy
// sector.tmp paths so older workers do not recycle new attempts' scratch space.
func SDRTempRoot(sectorPath string) string {
	return sectorPath + ".sdr" + TempSuffix
}
