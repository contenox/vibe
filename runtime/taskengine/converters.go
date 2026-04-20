package taskengine

func NormalizeFinalChainOutput(value any, dt DataType) (any, DataType, error) {
	return NormalizeDataType(value, dt)
}
