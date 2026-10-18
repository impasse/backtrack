const errorMessages: Record<string, string> = {
	unauthorized: '登录状态已失效，请重新登录。',
	'invalid access token': '访问令牌无效，请检查后重试。',
	'database unavailable': '数据库暂时不可用，请稍后重试。',
	'clip service is unavailable': '片段导出服务暂时不可用，请稍后重试。',
	'playlist not found': '找不到这段播放内容，请刷新页面后重试。',
	'media not found': '找不到对应的录像文件，可能已按保留策略清理。',
	'clip is not ready': '片段仍在处理中，请稍后再试。',
	'session is still active; use its live or dvr playlist':
		'这场直播仍在进行，请使用直播或时移回放。',
	'clip queue is full': '导出任务较多，请稍后再提交。',
	'clip exceeds configured maximum duration': '所选片段超过了系统允许的最长时长。',
	'there are no committed segments in the requested range': '所选时间范围内没有可导出的录像。',
	'selected range does not overlap committed media': '所选时间范围内没有可用录像，请调整起止时间。',
	'ffmpeg is required for clip export': '服务器未安装 FFmpeg，暂时无法导出片段。',
	'ffprobe is required to validate exported clips': '服务器未安装 ffprobe，暂时无法校验导出文件。',
	'export exceeds configured output size limit': '导出文件超过了系统允许的大小。',
	'content-type must be application/sdp': '推流请求格式不受支持，请重新发起推流。',
	'publish key required': '缺少推流码，请重新打开推流窗口。',
	'stream must include h.264, hevc, or av1 video':
		'未检测到受支持的视频编码，请在 OBS 中选择 H.264、HEVC 或 AV1。',
};

/** Convert low-level failures into concise messages suitable for the interface. */
export function userFacingError(error: unknown, fallback = '操作未能完成，请稍后重试。'): string {
	if (error instanceof TypeError)
		return '无法连接 Backtrack 服务，请检查服务状态或网络连接后重试。';

	const message = error instanceof Error ? error.message.trim() : String(error ?? '').trim();
	if (!message) return fallback;
	const normalized = message.toLowerCase();

	for (const [source, translated] of Object.entries(errorMessages)) {
		if (normalized.includes(source)) return translated;
	}

	if (normalized.includes('requested range contains a recording gap')) {
		return '所选区间包含录制中断，请缩短区间或调整缺口处理方式。';
	}
	if (normalized.includes('requested end is beyond the committed media frontier')) {
		return '结束时间晚于当前已录制内容，请缩短剪辑区间。';
	}
	if (/^request failed \(\d+\)$/.test(normalized)) {
		return '服务暂时无法完成请求，请稍后重试。';
	}
	if (/^[\x00-\x7f]*$/.test(message)) return fallback;
	return message;
}

export function clipStatusLabel(status: string): string {
	const labels: Record<string, string> = {
		QUEUED: '排队中',
		RUNNING: '正在导出',
		COMPLETED: '已完成',
		FAILED: '导出失败',
		CANCELLED: '已取消',
	};
	return labels[status] ?? '状态未知';
}

export function playbackErrorMessage(details: string): string {
	const labels: Record<string, string> = {
		levelEmptyError: '暂时没有可播放的录像片段，请稍后重试。',
		networkError: '播放连接中断，请检查服务或网络后重试。',
		mediaError: '浏览器无法解码这段媒体，请确认设备支持该视频格式。',
	};
	return labels[details] ?? '播放失败，请刷新页面后重试。';
}
