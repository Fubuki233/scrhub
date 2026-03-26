/**
 * Audio decoder using WebCodecs API for web-scrcpy.
 */

const PACKET_HEADER_SIZE = 12;
const FLAG_CONFIG = BigInt(1) << BigInt(63);

class AudioPlayer {
    constructor() {
        this.decoder = null;
        this.audioCtx = null;
        this.codecString = null;
        this.configData = null;
        this.muted = true;
        this.sampleRate = 48000;
        this.channels = 2;
        this.ready = false;

        // Audio buffer queue
        this.pendingBuffers = [];
        this.nextPlayTime = 0;
    }

    /**
     * Initialize with codec info.
     * @param {string} codec - WebCodecs codec string (e.g. "opus")
     */
    init(codec) {
        this.codecString = codec;

        if (typeof window.AudioDecoder === 'undefined') {
            console.warn('WebCodecs AudioDecoder not available');
            return;
        }

        this.audioCtx = new AudioContext({ sampleRate: this.sampleRate });

        this.decoder = new window.AudioDecoder({
            output: (audioData) => this._onAudioData(audioData),
            error: (e) => console.error('AudioDecoder error:', e),
        });
    }

    /**
     * Feed a raw scrcpy audio packet.
     * @param {Uint8Array} packet
     */
    feed(packet) {
        if (!this.decoder || packet.length < PACKET_HEADER_SIZE) return;

        const view = new DataView(packet.buffer, packet.byteOffset, packet.byteLength);
        const ptsHi = view.getUint32(0);
        const ptsLo = view.getUint32(4);
        const ptsFlags = (BigInt(ptsHi) << BigInt(32)) | BigInt(ptsLo);
        const payloadSize = view.getUint32(8);

        const isConfig = (ptsFlags & FLAG_CONFIG) !== BigInt(0);
        const payload = packet.subarray(PACKET_HEADER_SIZE, PACKET_HEADER_SIZE + payloadSize);

        if (isConfig) {
            this.configData = new Uint8Array(payload);
            this._configure();
            return;
        }

        if (this.decoder.state !== 'configured') return;
        if (this.muted) return;

        const pts = ptsFlags & ((BigInt(1) << BigInt(62)) - BigInt(1));

        const chunk = new EncodedAudioChunk({
            type: 'key', // audio chunks are always key frames
            timestamp: Number(pts),
            data: payload,
        });

        try {
            this.decoder.decode(chunk);
        } catch (e) {
            console.warn('audio decode error:', e);
        }
    }

    _configure() {
        if (!this.decoder || !this.codecString) return;

        const config = {
            codec: this.codecString,
            sampleRate: this.sampleRate,
            numberOfChannels: this.channels,
        };

        if (this.configData) {
            config.description = this.configData;
        }

        try {
            this.decoder.configure(config);
            this.ready = true;
        } catch (e) {
            console.error('Failed to configure AudioDecoder:', e);
        }
    }

    _onAudioData(audioData) {
        if (this.muted || !this.audioCtx) {
            audioData.close();
            return;
        }

        const numFrames = audioData.numberOfFrames;
        const numChannels = audioData.numberOfChannels;

        const buffer = this.audioCtx.createBuffer(numChannels, numFrames, audioData.sampleRate);

        for (let ch = 0; ch < numChannels; ch++) {
            const channelData = new Float32Array(numFrames);
            audioData.copyTo(channelData, { planeIndex: ch, format: 'f32-planar' });
            buffer.copyToChannel(channelData, ch);
        }
        audioData.close();

        // Schedule playback
        const source = this.audioCtx.createBufferSource();
        source.buffer = buffer;
        source.connect(this.audioCtx.destination);

        const now = this.audioCtx.currentTime;
        const startTime = Math.max(now, this.nextPlayTime);
        source.start(startTime);
        this.nextPlayTime = startTime + buffer.duration;
    }

    setMuted(muted) {
        this.muted = muted;
        if (!muted && this.audioCtx && this.audioCtx.state === 'suspended') {
            this.audioCtx.resume();
        }
    }

    destroy() {
        if (this.decoder && this.decoder.state !== 'closed') {
            this.decoder.close();
        }
        if (this.audioCtx) {
            this.audioCtx.close();
        }
    }
}

export { AudioPlayer };
