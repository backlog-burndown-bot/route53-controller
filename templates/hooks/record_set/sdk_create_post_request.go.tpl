
	// Retry transient InvalidChangeBatch failures instead of going terminal (community#2754).
	err = requeueOnTransientChangeBatchError(err)
