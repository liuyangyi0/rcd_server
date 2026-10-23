use rcd_communicate::frame::{self, FrameDecoder, Limits};

const LIMITS: Limits = Limits {
    frame: 4096,
    payload: 65535,
};
fn recheck(frame: &mut [u8]) {
    let end = frame.len() - 4;
    let crc = crc32fast::hash(&frame[..end]);
    frame[end..].copy_from_slice(&crc.to_le_bytes());
}

#[test]
fn crc_standard_vector_and_existing_zlib_envelope() {
    assert_eq!(crc32fast::hash(b"123456789"), 0xcbf43926);
    // Independently specified zlib stream for "hello" (not output of encode).
    let mut frame = vec![
        23, 0, 5, 0, 13, 0, 0x78, 0x9c, 0xcb, 0x48, 0xcd, 0xc9, 0xc9, 0x07, 0x00, 0x06, 0x2c, 0x02,
        0x15, 0, 0, 0, 0,
    ];
    recheck(&mut frame);
    assert_eq!(frame::decode(&frame, LIMITS).unwrap(), b"hello");
}

#[test]
fn all_chunk_boundaries_and_consecutive_frames() {
    let raw = frame::encode(b"hello, frame", LIMITS).unwrap();
    for split in 0..=raw.len() {
        let mut decoder = FrameDecoder::new(LIMITS).unwrap();
        let mut out = decoder.feed(&raw[..split]).unwrap().payloads;
        out.extend(decoder.feed(&raw[split..]).unwrap().payloads);
        assert_eq!(out, vec![b"hello, frame".to_vec()]);
    }
    let joined = [raw.as_slice(), raw.as_slice()].concat();
    assert_eq!(
        FrameDecoder::new(LIMITS)
            .unwrap()
            .feed(&joined)
            .unwrap()
            .payloads
            .len(),
        2
    );
}

#[test]
fn bad_lengths_crc_expansion_truncation_and_trailing_stream_are_rejected() {
    let original = frame::encode(&[42; 200], LIMITS).unwrap();
    for size in 0..original.len() {
        assert!(frame::decode(&original[..size], LIMITS).is_err());
    }
    let mut bad = original.clone();
    bad[4..6].copy_from_slice(&u16::MAX.to_le_bytes());
    recheck(&mut bad);
    assert!(frame::decode(&bad, LIMITS).is_err());
    bad = original.clone();
    bad[8] ^= 1;
    assert!(frame::decode(&bad, LIMITS).is_err());
    bad = original.clone();
    bad[2..4].copy_from_slice(&1u16.to_le_bytes());
    recheck(&mut bad);
    assert!(frame::decode(&bad, LIMITS).is_err());
    // Cut the zlib trailer, then make header and CRC self-consistent. The
    // decoder must demand StreamEnd, not accept partial decompression output.
    bad = original.clone();
    bad.remove(bad.len() - 5);
    let size = bad.len();
    bad[..2].copy_from_slice(&(size as u16).to_le_bytes());
    bad[4..6].copy_from_slice(&((size - 10) as u16).to_le_bytes());
    recheck(&mut bad);
    assert!(frame::decode(&bad, LIMITS).is_err());
    bad = original.clone();
    bad.insert(bad.len() - 4, 0);
    let size = bad.len();
    bad[..2].copy_from_slice(&(size as u16).to_le_bytes());
    bad[4..6].copy_from_slice(&((size - 10) as u16).to_le_bytes());
    recheck(&mut bad);
    assert!(frame::decode(&bad, LIMITS).is_err());
}

#[test]
fn noise_recovers_and_timeout_reset_drops_partial_frame() {
    let raw = frame::encode(b"reply", LIMITS).unwrap();
    let mut decoder = FrameDecoder::new(LIMITS).unwrap();
    let input = [&[0xff; 17][..], &raw].concat();
    let result = decoder.feed(&input).unwrap();
    assert_eq!(result.payloads, vec![b"reply".to_vec()]);
    assert_eq!(result.discarded_bytes, 17);
    decoder.feed(&raw[..8]).unwrap();
    decoder.reset();
    assert_eq!(decoder.feed(&raw).unwrap().payloads.len(), 1);
    assert!(decoder.feed(&vec![0; LIMITS.frame + 1]).is_err());
    assert_eq!(decoder.feed(&raw).unwrap().payloads.len(), 1);
}

#[test]
fn malformed_input_never_panics_or_grows_without_limit() {
    let limits = Limits {
        frame: 256,
        payload: 512,
    };
    let mut decoder = FrameDecoder::new(limits).unwrap();
    let mut seed = 0x12345678u32;
    for _ in 0..2000 {
        let mut bytes = vec![0; 64];
        for byte in &mut bytes {
            seed ^= seed << 13;
            seed ^= seed >> 17;
            seed ^= seed << 5;
            *byte = seed as u8;
        }
        let _ = frame::decode(&bytes, limits);
        let _ = decoder.feed(&bytes);
    }
}

#[test]
fn expired_noise_header_recovers_complete_frames_not_unverified_suffixes() {
    let raw = frame::encode(b"valid peer payload", LIMITS).unwrap();
    let mut decoder = FrameDecoder::new(LIMITS).unwrap();
    let prefix = [0x00, 0x10, 0x69, 0x00, 0xf6, 0x0f];
    let input = [prefix.as_slice(), &raw, &raw, &[0xff, 0xaa]].concat();
    assert!(decoder.feed(&input).unwrap().payloads.is_empty());
    let recovered = decoder.recover_expired();
    assert_eq!(recovered.payloads, vec![b"valid peer payload".to_vec(); 2]);
    assert_eq!(recovered.discarded_bytes, 8);
    assert!(!decoder.has_partial_frame());
    assert_eq!(decoder.feed(&raw).unwrap().payloads.len(), 1);
}
