/**
 * MSE (Media Source Extensions) based H.264 video decoder for web-scrcpy.
 * Used as fallback when WebCodecs API is not available (non-secure context).
 * Converts H.264 Annex B stream → fMP4 → MSE SourceBuffer → <video> element.
 *
 * Recovery strategy: when the decoder stalls (missing reference frames from
 * dropped packets), the entire MSE pipeline is torn down and rebuilt from the
 * next keyframe. This guarantees a clean decode chain.
 */

import { parseAnnexBNALUs, createInitSegment, createMediaSegment, annexBToAvcc } from './mp4mux.js';

const PACKET_HEADER_SIZE = 12;
const FLAG_CONFIG = BigInt(1) << BigInt(63);
const FLAG_KEY_FRAME = BigInt(1) << BigInt(62);
const PTS_MASK = FLAG_KEY_FRAME - BigInt(1);

const NAL_SPS = 7;
const NAL_PPS = 8;

const TIMESCALE = 90000;

class MSEDecoder {
    constructor(videoElement) {
        this.video = videoElement;
        this.mediaSource = null;
        this.sourceBuffer = null;
        this.sps = null;
        this.pps = null;
        this.initialized = false;
        this.sequenceNumber = 1;
        this.baseTime = -1;
        this.lastPts = 0;
        this.width = 0;
        this.height = 0;
        this.frameCount = 0;
        this.ready = false;
        this.pendingBuffers = [];
        this.appending = false;
        this.codecString = '';

        // Recovery state: when true, drop all P-frames until next keyframe
        this._waitingForKeyframe = false;
        this._resetCount = 0;

        // Stall detection
        this._lastPlaybackTime = -1;
        this._stallStartTime = 0;
        this._stallCheckInterval = null;
        this._lastTrimTime = 0;
        this._catchUpActive = false;

        // Autoplay state
        this._playBlocked = false;
        this.onPlayBlocked = null;
    }

    init(codec, width, height) {
        this.width = width;
        this.height = height;
        this.codecString = codec;
        console.log(`[mse] init ${width}x${height} codec=${codec}`);
    }

    feed(packet) {
        if (packet.length < PACKET_HEADER_SIZE) return;

        const view = new DataView(packet.buffer, packet.byteOffset, packet.byteLength);
        const ptsHi = view.getUint32(0);
        const ptsLo = view.getUint32(4);
        const ptsFlags = (BigInt(ptsHi) << BigInt(32)) | BigInt(ptsLo);
        const payloadSize = view.getUint32(8);

        const isConfig = (ptsFlags & FLAG_CONFIG) !== BigInt(0);
        const isKeyFrame = (ptsFlags & FLAG_KEY_FRAME) !== BigInt(0);
        const pts = Number(ptsFlags & PTS_MASK);

        const payload = packet.subarray(PACKET_HEADER_SIZE, PACKET_HEADER_SIZE + payloadSize);

        // --- Handle config (SPS/PPS) packets ---
        if (isConfig) {
            const nalus = parseAnnexBNALUs(payload);
            for (const nalu of nalus) {
                const naluType = nalu[0] & 0x1F;
                if (naluType === NAL_SPS) this.sps = new Uint8Array(nalu);
                if (naluType === NAL_PPS) this.pps = new Uint8Array(nalu);
            }
            if (this.sps && this.pps) {
                const profile = this.sps[1], compat = this.sps[2], level = this.sps[3];
                this.codecString = 'avc1.' +
                    profile.toString(16).padStart(2, '0') +
                    compat.toString(16).padStart(2, '0') +
                    level.toString(16).padStart(2, '0');
            }
            return;
        }

        // --- If waiting for keyframe after a reset, drop P-frames ---
        if (this._waitingForKeyframe) {
            if (!isKeyFrame) return; // drop until we get a clean start
            console.log(`[mse] keyframe arrived after reset, reinitializing`);
            this._waitingForKeyframe = false;
            this._doInitMSE();
        }
        // --- First-time init: wait for SPS/PPS + first keyframe ---
        else if (!this.initialized && !this.sourceBuffer) {
            if (!isKeyFrame || !this.sps || !this.pps) return;
            console.log(`[mse] first keyframe received, initializing MSE`);
            this._doInitMSE();
        }

        // Build the media segment even if sourceBuffer isn't ready yet
        // — it will be queued in pendingBuffers
        if (!this.sps || !this.pps) return;

        const avccData = annexBToAvcc(payload, true);
        if (avccData.length === 0) return;

        if (this.baseTime < 0) {
            this.baseTime = pts;
        }

        const relativeTime = pts - this.baseTime;
        const decodeTime = Math.floor(relativeTime * TIMESCALE / 1000000);
        const duration = this.lastPts > 0
            ? Math.max(1, Math.floor((pts - this.lastPts) * TIMESCALE / 1000000))
            : Math.floor(TIMESCALE / 60);
        this.lastPts = pts;

        const segment = createMediaSegment(this.sequenceNumber++, decodeTime, [{
            data: avccData,
            duration: duration,
            isKey: isKeyFrame,
        }]);

        this._appendBuffer(segment);
        this.frameCount++;
    }

    /**
     * Initialize (or reinitialize) the full MSE pipeline.
     */
    _doInitMSE() {
        // Tear down any previous MSE state
        this._teardownMSE();

        if (!this.sps || !this.pps) {
            this._waitingForKeyframe = true;
            return;
        }

        const mimeType = `video/mp4; codecs="${this.codecString}"`;
        if (!MediaSource.isTypeSupported(mimeType)) {
            console.error(`[mse] unsupported: ${mimeType}`);
            return;
        }

        this.mediaSource = new MediaSource();
        this.video.src = URL.createObjectURL(this.mediaSource);

        this.mediaSource.addEventListener('sourceopen', () => {
            try {
                this.sourceBuffer = this.mediaSource.addSourceBuffer(mimeType);
                this.sourceBuffer.mode = 'sequence';

                this.sourceBuffer.addEventListener('updateend', () => {
                    this.appending = false;
                    this._processPendingBuffers();
                });

                this.sourceBuffer.addEventListener('error', (e) => {
                    console.error('[mse] SourceBuffer error:', e);
                    this.appending = false;
                });

                // Append init segment (ftyp + moov)
                const initSeg = createInitSegment(this.sps, this.pps, this.width, this.height, TIMESCALE);
                this.sourceBuffer.appendBuffer(initSeg);
                this.initialized = true;
                this.ready = true;

                // Attach recovery event listeners
                this.video.addEventListener('waiting', this._boundOnWaiting || (this._boundOnWaiting = () => this._onVideoWaiting()));
                this.video.addEventListener('error', this._boundOnError || (this._boundOnError = () => this._onVideoError()));
                // Chrome may pause "video-only background media" to save
                // power on non-secure contexts. Catch the pause and resume.
                this.video.addEventListener('pause', this._boundOnPause || (this._boundOnPause = () => this._onVideoPause()));

                // Assume blocked until play() succeeds — avoids race
                // condition with stall detector firing before .catch()
                this._playBlocked = true;
                this.video.play().then(() => {
                    console.log('[mse] autoplay succeeded');
                    this._playBlocked = false;
                }).catch((e) => {
                    console.warn('[mse] autoplay blocked:', e.message);
                    this._playBlocked = true;
                    if (this.onPlayBlocked) this.onPlayBlocked();
                });

                // Start stall detector (interval-based, unaffected by rAF throttling)
                this._startStallDetector();

                if (this._resetCount > 0) {
                    console.log(`[mse] pipeline reinitialized (reset #${this._resetCount})`);
                }
            } catch (e) {
                console.error('[mse] sourceopen error:', e);
            }
        });
    }

    /**
     * Tear down MediaSource/SourceBuffer without touching SPS/PPS or
     * dimensions. Prepares for a fresh _doInitMSE() call.
     */
    _teardownMSE() {
        if (this._stallCheckInterval) {
            clearInterval(this._stallCheckInterval);
            this._stallCheckInterval = null;
        }
        if (this.video) {
            if (this._boundOnWaiting) this.video.removeEventListener('waiting', this._boundOnWaiting);
            if (this._boundOnError) this.video.removeEventListener('error', this._boundOnError);
            if (this._boundOnPause) this.video.removeEventListener('pause', this._boundOnPause);
        }
        if (this.sourceBuffer) {
            try {
                if (this.mediaSource && this.mediaSource.readyState === 'open') {
                    this.sourceBuffer.abort();
                }
            } catch (e) { /* ignore */ }
        }
        if (this.mediaSource) {
            try {
                if (this.mediaSource.readyState === 'open') {
                    this.mediaSource.endOfStream();
                }
            } catch (e) { /* ignore */ }
        }
        // Revoke the old object URL to allow GC
        if (this.video.src && this.video.src.startsWith('blob:')) {
            URL.revokeObjectURL(this.video.src);
        }
        this.sourceBuffer = null;
        this.mediaSource = null;
        this.initialized = false;
        this.pendingBuffers = [];
        this.appending = false;
        this.baseTime = -1;
        this.lastPts = 0;
        this.sequenceNumber = 1;
        this._stallStartTime = 0;
        this._lastPlaybackTime = -1;
        this._catchUpActive = false;
    }

    /**
     * FULL RESET: destroy MSE pipeline, drop frames until next keyframe,
     * then reinitialize from that keyframe.
     */
    _resetPipeline(reason) {
        this._resetCount++;
        console.warn(`[mse] RESET #${this._resetCount}: ${reason}`);
        this._teardownMSE();
        this._waitingForKeyframe = true;
        // MSE will be reinitialized when the next keyframe arrives in feed()
    }

    _appendBuffer(data) {
        this.pendingBuffers.push(data);
        this._processPendingBuffers();
    }

    _processPendingBuffers() {
        if (this.appending || !this.sourceBuffer || this.pendingBuffers.length === 0) return;
        if (this.sourceBuffer.updating) return;

        // Batch all pending buffers into one append
        let totalLen = 0;
        for (let i = 0; i < this.pendingBuffers.length; i++) totalLen += this.pendingBuffers[i].length;

        const batch = new Uint8Array(totalLen);
        let off = 0;
        for (let i = 0; i < this.pendingBuffers.length; i++) {
            batch.set(this.pendingBuffers[i], off);
            off += this.pendingBuffers[i].length;
        }
        this.pendingBuffers.length = 0;

        this.appending = true;
        try {
            this.sourceBuffer.appendBuffer(batch);
        } catch (e) {
            this.appending = false;
            if (e.name === 'QuotaExceededError') {
                this._trimBufferNow();
            } else {
                console.warn('[mse] append failed:', e.message);
                this._resetPipeline('append exception: ' + e.message);
            }
        }
    }

    // ======= VIDEO ELEMENT EVENTS =======

    _onVideoWaiting() {
        // Don't treat autoplay-blocked as a decoder stall
        if (this._playBlocked || this.video.paused) return;
        if (this._waitingTimer) return;
        this._waitingTimer = setTimeout(() => {
            this._waitingTimer = null;
            if (this._playBlocked || this.video.paused) return;
            if (this.video.readyState < 3 && this.initialized) {
                this._resetPipeline('video waiting timeout (decoder stuck)');
            }
        }, 500);
    }

    _onVideoError() {
        const err = this.video.error;
        if (err) {
            console.error(`[mse] video error: code=${err.code} msg=${err.message}`);
            this._resetPipeline('video element error');
        }
    }

    _onVideoPause() {
        // Chrome pauses "video-only background media" to save power.
        // Don't treat this as a stall — just resume.
        if (this.initialized && !this._playBlocked) {
            console.log('[mse] video paused (power saving?), resuming');
            this.video.play().then(() => {
                this._playBlocked = false;
            }).catch((e) => {
                console.warn('[mse] resume after pause blocked:', e.message);
                this._playBlocked = true;
                if (this.onPlayBlocked) this.onPlayBlocked();
            });
        }
    }

    // ======= STALL DETECTOR =======

    _startStallDetector() {
        if (this._stallCheckInterval) clearInterval(this._stallCheckInterval);
        this._lastPlaybackTime = this.video.currentTime;
        this._stallStartTime = 0;

        this._stallCheckInterval = setInterval(() => {
            if (!this.sourceBuffer || !this.initialized) return;

            // If video is paused, try to resume but NEVER trigger a
            // pipeline reset — pausing is not a decoder stall.
            if (this.video.paused) {
                if (!this._playBlocked) {
                    this.video.play().then(() => {
                        this._playBlocked = false;
                    }).catch((e) => {
                        this._playBlocked = true;
                        if (this.onPlayBlocked) this.onPlayBlocked();
                    });
                }
                // Reset stall tracking — being paused is not a stall
                this._stallStartTime = 0;
                this._lastPlaybackTime = -1;
                return;
            }

            const currentTime = this.video.currentTime;
            const buffered = this.sourceBuffer.buffered;
            if (buffered.length === 0) return;

            const end = buffered.end(buffered.length - 1);
            const latency = end - currentTime;

            // --- Smooth latency management ---
            if (latency > 0.8) {
                this.video.currentTime = end - 0.05;
                this.video.playbackRate = 1.0;
                this._catchUpActive = false;
            } else if (latency > 0.3) {
                if (!this._catchUpActive) {
                    this.video.playbackRate = 1.1;
                    this._catchUpActive = true;
                }
            } else {
                if (this._catchUpActive) {
                    this.video.playbackRate = 1.0;
                    this._catchUpActive = false;
                }
            }

            // --- Stall detection (only when video is actively playing) ---
            if (this._lastPlaybackTime >= 0 && Math.abs(currentTime - this._lastPlaybackTime) < 0.001) {
                if (latency > 0.05) {
                    if (this._stallStartTime === 0) {
                        this._stallStartTime = performance.now();
                    } else if (performance.now() - this._stallStartTime > 1500) {
                        this._resetPipeline(`stalled ${((performance.now() - this._stallStartTime)/1000).toFixed(1)}s`);
                        return;
                    }
                }
            } else {
                this._stallStartTime = 0;
            }
            this._lastPlaybackTime = currentTime;

            // Periodic trim
            const now = performance.now();
            if (now - this._lastTrimTime > 3000) {
                this._lastTrimTime = now;
                this._deferredTrim();
            }
        }, 200);
    }

    tryPlay() {
        if (!this.video) return;
        this.video.play().then(() => { this._playBlocked = false; }).catch(() => {});
    }

    _deferredTrim() {
        if (!this.sourceBuffer || this.sourceBuffer.updating || this.appending) return;
        try {
            const buffered = this.sourceBuffer.buffered;
            if (buffered.length === 0) return;
            const currentTime = this.video.currentTime;
            if (currentTime - buffered.start(0) > 2) {
                this.sourceBuffer.remove(buffered.start(0), currentTime - 0.5);
            }
        } catch (e) { /* ignore */ }
    }

    _trimBufferNow() {
        if (!this.sourceBuffer || this.sourceBuffer.updating) return;
        try {
            const buffered = this.sourceBuffer.buffered;
            if (buffered.length === 0) return;
            this.sourceBuffer.remove(buffered.start(0), this.video.currentTime - 0.2);
        } catch (e) { /* ignore */ }
    }

    get decoder() {
        if (this._waitingForKeyframe) return { state: 'recovering' };
        return this.sourceBuffer ? { state: this.initialized ? 'configured' : 'waiting' } : null;
    }

    destroy() {
        if (this._waitingTimer) { clearTimeout(this._waitingTimer); this._waitingTimer = null; }
        this._teardownMSE();
        this.sps = null;
        this.pps = null;
    }
}

export { MSEDecoder };
