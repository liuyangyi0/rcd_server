//! Per-port control transaction spacing. This never sleeps or performs I/O:
//! the owning worker continues acquisition while its next control is waiting.
use std::time::{Duration, Instant};

#[derive(Default)]
pub(super) struct CommandPacing {
    interval: Duration,
    ready_at: Option<Instant>,
}

impl CommandPacing {
    pub(super) fn new(interval: Duration) -> Self {
        Self {
            interval,
            ready_at: None,
        }
    }

    pub(super) fn ready(&self, now: Instant) -> bool {
        self.ready_at.is_none_or(|deadline| now >= deadline)
    }

    pub(super) fn completed(&mut self, now: Instant) {
        self.ready_at = if self.interval.is_zero() {
            None
        } else {
            Some(now + self.interval)
        };
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn command_interval_zero_preserves_immediate_dispatch() {
        let now = Instant::now();
        let mut pacing = CommandPacing::default();
        pacing.completed(now);
        assert!(pacing.ready(now));
    }

    #[test]
    fn command_interval_is_per_port_and_measured_from_completion() {
        let start = Instant::now();
        let mut first = CommandPacing::new(Duration::from_millis(500));
        let other = CommandPacing::new(Duration::from_millis(500));
        assert!(
            first.ready(start),
            "the first control has no artificial delay"
        );
        let finished = start + Duration::from_secs(2);
        first.completed(finished);
        assert!(!first.ready(finished + Duration::from_millis(499)));
        assert!(first.ready(finished + Duration::from_millis(500)));
        assert!(
            other.ready(finished),
            "another port must remain independent"
        );
    }
}
