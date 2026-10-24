//! A single owner of a physical serial handle. No network or OPC caller writes
//! directly to this handle, and partial writes are never retried from byte zero.
use crate::config::{Parity, PortConfig};
use serialport::{DataBits, FlowControl, SerialPort, StopBits};
use std::io::{self, Read, Write};
use std::time::{Duration, Instant};
use thiserror::Error;
use tokio_util::sync::CancellationToken;

const IO_SLICE: Duration = Duration::from_millis(10);

pub trait Transport: Send {
    fn read(&mut self, buffer: &mut [u8], timeout: Duration) -> io::Result<usize>;
    fn write(&mut self, buffer: &[u8], timeout: Duration) -> io::Result<usize>;
    fn clear_input(&mut self) -> io::Result<()>;
}

pub struct NativeSerial {
    port: Box<dyn SerialPort>,
}

impl NativeSerial {
    pub fn open(config: &PortConfig) -> io::Result<Self> {
        let path = config.device.canonicalize()?;
        let path = path
            .to_str()
            .ok_or_else(|| io::Error::other("serial path is not UTF-8"))?;
        let bits = match config.data_bits {
            5 => DataBits::Five,
            6 => DataBits::Six,
            7 => DataBits::Seven,
            8 => DataBits::Eight,
            _ => return Err(io::Error::other("invalid data bits")),
        };
        let stop = match config.stop_bits {
            1 => StopBits::One,
            2 => StopBits::Two,
            _ => return Err(io::Error::other("invalid stop bits")),
        };
        let parity = match config.parity {
            Parity::None => serialport::Parity::None,
            Parity::Even => serialport::Parity::Even,
            Parity::Odd => serialport::Parity::Odd,
        };
        let builder = serialport::new(path, config.baud_rate)
            .data_bits(bits)
            .stop_bits(stop)
            .parity(parity)
            .flow_control(FlowControl::None)
            .timeout(IO_SLICE);
        #[cfg(unix)]
        let port: Box<dyn SerialPort> = {
            let mut native = builder.open_native()?;
            native.set_exclusive(true)?;
            Box::new(native)
        };
        #[cfg(not(unix))]
        let port = builder.open()?;
        Ok(Self { port })
    }
}

impl Transport for NativeSerial {
    fn read(&mut self, buffer: &mut [u8], timeout: Duration) -> io::Result<usize> {
        self.port.set_timeout(timeout.min(IO_SLICE))?;
        self.port.read(buffer)
    }
    fn write(&mut self, buffer: &[u8], timeout: Duration) -> io::Result<usize> {
        self.port.set_timeout(timeout.min(IO_SLICE))?;
        self.port.write(buffer)
    }
    fn clear_input(&mut self) -> io::Result<()> {
        self.port
            .clear(serialport::ClearBuffer::Input)
            .map_err(Into::into)
    }
}

#[derive(Debug, Error)]
pub enum WriteError {
    #[error("serial write cancelled after {written} bytes")]
    Cancelled { written: usize },
    #[error("serial write timed out after {written} bytes; delivery may be partial")]
    Timeout { written: usize },
    #[error("serial write failed after {written} bytes: {source}")]
    Io { written: usize, source: io::Error },
}

pub fn write_once(
    transport: &mut dyn Transport,
    bytes: &[u8],
    deadline: Instant,
    cancel: &CancellationToken,
) -> Result<(), WriteError> {
    let mut written = 0;
    while written < bytes.len() {
        if cancel.is_cancelled() {
            return Err(WriteError::Cancelled { written });
        }
        let remaining = deadline.saturating_duration_since(Instant::now());
        if remaining.is_zero() {
            return Err(WriteError::Timeout { written });
        }
        match transport.write(&bytes[written..], remaining.min(IO_SLICE)) {
            Ok(0) => {
                return Err(WriteError::Io {
                    written,
                    source: io::Error::new(io::ErrorKind::WriteZero, "zero-length serial write"),
                })
            }
            Ok(n) if n <= bytes.len() - written => written += n,
            Ok(_) => {
                return Err(WriteError::Io {
                    written,
                    source: io::Error::other("invalid transport write count"),
                })
            }
            Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
            Err(e)
                if matches!(
                    e.kind(),
                    io::ErrorKind::TimedOut | io::ErrorKind::WouldBlock
                ) =>
            {
                // A driver slice expiring is not the transaction deadline. Keep
                // the unsent suffix; never send the accepted prefix again.
                // WouldBlock may return immediately, so avoid a busy spin.
                std::thread::sleep(
                    deadline
                        .saturating_duration_since(Instant::now())
                        .min(Duration::from_millis(1)),
                );
            }
            Err(source) => return Err(WriteError::Io { written, source }),
        }
    }
    Ok(())
}

/// Discard only within a finite quarantine. An endless noise stream cannot hold
/// shutdown or the next deadline forever. No quarantined bytes reach decoders.
pub fn quarantine(
    transport: &mut dyn Transport,
    duration: Duration,
    cancel: &CancellationToken,
) -> io::Result<()> {
    transport.clear_input()?;
    let end = Instant::now() + duration;
    let mut bytes = [0u8; 1024];
    while !cancel.is_cancelled() && Instant::now() < end {
        match transport.read(
            &mut bytes,
            end.saturating_duration_since(Instant::now()).min(IO_SLICE),
        ) {
            Ok(0) => {
                return Err(io::Error::new(
                    io::ErrorKind::UnexpectedEof,
                    "serial closed during quarantine",
                ))
            }
            Ok(_) => {}
            Err(e)
                if matches!(
                    e.kind(),
                    io::ErrorKind::TimedOut
                        | io::ErrorKind::WouldBlock
                        | io::ErrorKind::Interrupted
                ) => {}
            Err(e) => return Err(e),
        }
    }
    transport.clear_input()
}
