import { userFacingError } from '#lib/messages';

export const browserPublish = $state({
	active: false,
	busy: false,
	error: '',
	label: '',
	streamId: '',
});

let peer: RTCPeerConnection | undefined;
let capture: MediaStream | undefined;
let sessionLocation = '';
let manuallyStopping = false;
let publishRun = 0;

type StartOptions = {
	streamId: string;
	publishKey: string;
	mode: 'screen' | 'camera';
	frameRate: number;
	resolution: '1080p' | '720p' | 'original';
};

function waitForICE(connection: RTCPeerConnection) {
	if (connection.connectionState === 'closed') return Promise.reject(new Error('推流已取消'));
	if (connection.iceGatheringState === 'complete') return Promise.resolve();
	return new Promise<void>((resolve, reject) => {
		const timeout = window.setTimeout(() => reject(new Error('ICE 候选收集超时')), 20000);
		connection.addEventListener('icegatheringstatechange', () => {
			if (connection.iceGatheringState === 'complete') {
				window.clearTimeout(timeout);
				resolve();
			}
		});
		connection.addEventListener('connectionstatechange', () => {
			if (connection.connectionState === 'failed') {
				window.clearTimeout(timeout);
				reject(new Error('WebRTC 连接失败'));
			} else if (connection.connectionState === 'closed') {
				window.clearTimeout(timeout);
				reject(new Error('推流已取消'));
			}
		});
	});
}

function closeLocalMedia() {
	const currentPeer = peer;
	peer = undefined;
	currentPeer?.close();
	capture?.getTracks().forEach((track) => track.stop());
	capture = undefined;
}

async function deleteSession(keepalive = false) {
	const location = sessionLocation;
	sessionLocation = '';
	if (!location) return;
	try {
		await fetch(location, { method: 'DELETE', credentials: 'same-origin', keepalive });
	} catch {
		// Closing the document can prevent network cleanup; the server also detects ICE loss.
	}
}

export async function startBrowserPublish(options: StartOptions) {
	if (browserPublish.busy || browserPublish.active) return;
	const run = ++publishRun;
	browserPublish.busy = true;
	browserPublish.error = '';
	browserPublish.streamId = options.streamId;
	try {
		if (!window.isSecureContext) throw new Error('浏览器推流需要 HTTPS 或 localhost');
		const videoConstraints: MediaTrackConstraints = {
			frameRate: { ideal: options.frameRate, max: options.frameRate },
		};
		if (options.resolution !== 'original') {
			const maxDimension =
				options.resolution === '1080p'
					? { width: 1920, height: 1080 }
					: { width: 1280, height: 720 };
			videoConstraints.width = { ideal: maxDimension.width, max: maxDimension.width };
			videoConstraints.height = { ideal: maxDimension.height, max: maxDimension.height };
		}
		capture =
			options.mode === 'screen'
				? await navigator.mediaDevices.getDisplayMedia({ video: videoConstraints, audio: true })
				: await navigator.mediaDevices.getUserMedia({ video: videoConstraints, audio: true });
		if (run !== publishRun) throw new Error('推流已取消');
		for (const track of capture.getTracks()) {
			track.addEventListener('ended', () => {
				if (manuallyStopping) return;
				browserPublish.error = '采集来源已结束，推流已停止';
				void stopBrowserPublish();
			}, { once: true });
		}
		if (!capture.getAudioTracks().length) {
			try {
				const mic = await navigator.mediaDevices.getUserMedia({ audio: true });
				if (run !== publishRun) {
					mic.getTracks().forEach((track) => track.stop());
					throw new Error('推流已取消');
				}
				for (const track of mic.getAudioTracks()) {
					capture.addTrack(track);
					track.addEventListener('ended', () => {
						if (!manuallyStopping) {
							browserPublish.error = '音频采集已结束，推流已停止';
							void stopBrowserPublish();
						}
					}, { once: true });
				}
			} catch {
				// Audio is optional when microphone permission is unavailable.
			}
		}
		if (run !== publishRun) throw new Error('推流已取消');
		const connection = new RTCPeerConnection({ iceCandidatePoolSize: 1 });
		peer = connection;
		for (const track of capture.getTracks()) connection.addTrack(track, capture);
		const offer = await connection.createOffer();
		if (run !== publishRun) throw new Error('推流已取消');
		await connection.setLocalDescription(offer);
		if (run !== publishRun) throw new Error('推流已取消');
		await waitForICE(connection);
		if (run !== publishRun) throw new Error('推流已取消');
		const response = await fetch(`/api/whip/${encodeURIComponent(options.streamId)}`, {
			method: 'POST',
			headers: {
				'Content-Type': 'application/sdp',
				Authorization: `Bearer ${options.publishKey.trim()}`,
			},
			body: connection.localDescription?.sdp,
			credentials: 'same-origin',
		});
		if (!response.ok) {
			const body = await response.json().catch(() => ({}));
			throw new Error(body.error ?? `推流协商失败 (${response.status})`);
		}
		sessionLocation = response.headers.get('Location') ?? '';
		if (run !== publishRun) throw new Error('推流已取消');
		await connection.setRemoteDescription({ type: 'answer', sdp: await response.text() });
		if (run !== publishRun) throw new Error('推流已取消');
		browserPublish.active = true;
		browserPublish.label = options.mode === 'screen' ? '屏幕' : '摄像头';
		connection.addEventListener('connectionstatechange', () => {
			if (connection.connectionState === 'failed')
				browserPublish.error = 'WebRTC 连接中断，请停止后重新推流';
		});
	} catch (error) {
		if (run === publishRun) browserPublish.error = userFacingError(error);
		await deleteSession();
		closeLocalMedia();
		if (run === publishRun) browserPublish.streamId = '';
	} finally {
		browserPublish.busy = false;
	}
}

export async function stopBrowserPublish(keepalive = false) {
	manuallyStopping = true;
	publishRun++;
	closeLocalMedia();
	await deleteSession(keepalive);
	browserPublish.active = false;
	browserPublish.label = '';
	browserPublish.streamId = '';
	manuallyStopping = false;
}

export function notifyBrowserPublishEnded() {
	publishRun++;
	closeLocalMedia();
	browserPublish.active = false;
	browserPublish.busy = false;
	browserPublish.label = '';
	browserPublish.streamId = '';
	void deleteSession(true);
}
