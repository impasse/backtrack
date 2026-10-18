export type Stream = {
	id: string;
	name: string;
	enabled: boolean;
	createdAt: string;
	publishKey?: string;
};
export type TimeRange = {
	start: string;
	end: string;
	available: boolean;
	reason?: string;
	sessionId?: string;
	segmentIds?: string[];
	pinned?: boolean;
};
export type ClipJob = {
	id: string;
	request: {
		name?: string;
		streamId: string;
		startTime: string;
		endTime: string;
		mode: string;
		gapPolicy: string;
		saveToLibrary?: boolean;
	};
	status: string;
	progress: number;
	error?: string;
	pinned?: boolean;
	createdAt: string;
};
