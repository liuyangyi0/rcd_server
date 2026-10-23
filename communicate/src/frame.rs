//! Existing communicate envelope: three LE u16 lengths, zlib data, CRC32 LE.
//! This does not define the device-specific request or business payload.
use flate2::{write::ZlibEncoder, Compression, Decompress, FlushDecompress, Status};
use std::io::Write;
use thiserror::Error;

#[derive(Clone, Copy, Debug)]
pub struct Limits {
    pub frame: usize,
    pub payload: usize,
}

#[derive(Debug, Error)]
pub enum FrameError {
    #[error("invalid frame length or configured limit")]
    Length,
    #[error("CRC32 mismatch")]
    Checksum,
    #[error("invalid compressed stream: {0}")]
    Compression(#[from] std::io::Error),
    #[error("zlib decode failed: {0}")]
    Decompression(#[from] flate2::DecompressError),
    #[error("decompressed length or compressed stream boundary mismatch")]
    PayloadLength,
    #[error("stream input exceeds bounded frame buffer")]
    BufferLimit,
}

impl Limits {
    pub fn validate(self) -> Result<(), FrameError> {
        if !(10..=u16::MAX as usize).contains(&self.frame)
            || !(1..=u16::MAX as usize).contains(&self.payload)
        {
            return Err(FrameError::Length);
        }
        Ok(())
    }
}
fn u16_at(bytes: &[u8], offset: usize) -> usize {
    u16::from_le_bytes([bytes[offset], bytes[offset + 1]]) as usize
}
fn valid_header(bytes: &[u8], limits: Limits) -> bool {
    let total = u16_at(bytes, 0);
    let plain = u16_at(bytes, 2);
    let compressed = u16_at(bytes, 4);
    (10..=limits.frame).contains(&total) && plain <= limits.payload && compressed + 10 == total
}

pub fn encode(payload: &[u8], limits: Limits) -> Result<Vec<u8>, FrameError> {
    limits.validate()?;
    if payload.len() > limits.payload {
        return Err(FrameError::Length);
    }
    let mut encoder = ZlibEncoder::new(Vec::new(), Compression::new(2));
    encoder.write_all(payload)?;
    let compressed = encoder.finish()?;
    // Workbook short frames (01/91, header-only examples) are uncompressed.
    // Equal stored/plain lengths indicate raw bytes, not a zlib stream.
    let compressed = if compressed.len() >= payload.len() {
        payload.to_vec()
    } else {
        compressed
    };
    let total = compressed.len() + 10;
    if total > limits.frame {
        return Err(FrameError::Length);
    }
    let mut frame = Vec::with_capacity(total);
    frame.extend_from_slice(&(total as u16).to_le_bytes());
    frame.extend_from_slice(&(payload.len() as u16).to_le_bytes());
    frame.extend_from_slice(&(compressed.len() as u16).to_le_bytes());
    frame.extend_from_slice(&compressed);
    frame.extend_from_slice(&crc32fast::hash(&frame).to_le_bytes());
    Ok(frame)
}

pub fn decode(frame: &[u8], limits: Limits) -> Result<Vec<u8>, FrameError> {
    limits.validate()?;
    if frame.len() < 10 || !valid_header(frame, limits) || u16_at(frame, 0) != frame.len() {
        return Err(FrameError::Length);
    }
    let end = frame.len() - 4;
    let expected = u32::from_le_bytes(frame[end..].try_into().map_err(|_| FrameError::Length)?);
    if crc32fast::hash(&frame[..end]) != expected {
        return Err(FrameError::Checksum);
    }
    let declared = u16_at(frame, 2);
    if declared == end - 6 {
        return Ok(frame[6..end].to_vec());
    }
    let mut decoder = Decompress::new(true);
    let mut payload = vec![0; declared + 1];
    let status = decoder.decompress(&frame[6..end], &mut payload, FlushDecompress::Finish)?;
    if status != Status::StreamEnd
        || decoder.total_out() as usize != declared
        || decoder.total_in() as usize != end - 6
    {
        return Err(FrameError::PayloadLength);
    }
    payload.truncate(declared);
    Ok(payload)
}

#[derive(Default, Debug)]
pub struct FeedResult {
    pub payloads: Vec<Vec<u8>>,
    pub rejected_frames: u64,
    pub discarded_bytes: u64,
}
pub struct FrameDecoder {
    limits: Limits,
    buffer: Vec<u8>,
}

impl FrameDecoder {
    pub fn new(limits: Limits) -> Result<Self, FrameError> {
        limits.validate()?;
        Ok(Self {
            limits,
            buffer: Vec::new(),
        })
    }
    pub fn reset(&mut self) {
        self.buffer.clear();
    }

    pub fn has_partial_frame(&self) -> bool {
        !self.buffer.is_empty()
    }

    /// Once a candidate's deadline has expired, search past it for complete,
    /// CRC-verified frames. Never publish an unverified suffix or keep expired
    /// fragments indefinitely. A plausible noise header must not hide a peer frame.
    pub fn recover_expired(&mut self) -> FeedResult {
        let mut result = FeedResult::default();
        let mut start = 0;
        while self.buffer.len() - start >= 10 {
            let tail = &self.buffer[start..];
            if valid_header(tail, self.limits) {
                let total = u16_at(tail, 0);
                if total <= tail.len() {
                    match decode(&tail[..total], self.limits) {
                        Ok(payload) => {
                            result.payloads.push(payload);
                            start += total;
                            continue;
                        }
                        Err(_) => result.rejected_frames += 1,
                    }
                }
            }
            start += 1;
            result.discarded_bytes += 1;
        }
        result.discarded_bytes += (self.buffer.len() - start) as u64;
        self.buffer.clear();
        result
    }

    /// A plausible incomplete header waits until the transaction deadline.
    /// CRC alone cannot associate a complete stale reply with a request.
    pub fn feed(&mut self, chunk: &[u8]) -> Result<FeedResult, FrameError> {
        if chunk.len() > self.limits.frame
            || self.buffer.len() + chunk.len() > self.limits.frame * 2
        {
            self.reset();
            return Err(FrameError::BufferLimit);
        }
        self.buffer.extend_from_slice(chunk);
        let mut result = FeedResult::default();
        let mut start = 0;
        while self.buffer.len() - start >= 6 {
            let tail = &self.buffer[start..];
            if !valid_header(tail, self.limits) {
                start += 1;
                result.discarded_bytes += 1;
                continue;
            }
            let total = u16_at(tail, 0);
            if tail.len() < total {
                break;
            }
            match decode(&tail[..total], self.limits) {
                Ok(payload) => {
                    result.payloads.push(payload);
                    start += total;
                }
                Err(_) => {
                    result.rejected_frames += 1;
                    start += 1;
                    result.discarded_bytes += 1;
                }
            }
        }
        self.buffer.drain(..start);
        Ok(result)
    }
}
