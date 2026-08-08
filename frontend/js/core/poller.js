/**
 * Serial polling controller.
 *
 * A cycle is scheduled only after the previous request settles, so slow
 * responses can never overlap.  stop()/dispose() invalidate the active
 * sequence and abort the cycle signal; callers can safely ignore late API
 * responses even when the underlying HTTP client cannot cancel its fetch.
 */
export class Poller {
  constructor(task, {
    interval = 2000,
    hiddenInterval = 10000,
    maxInterval = 30000,
    onResult = () => true,
    onError = () => true,
  } = {}) {
    if (typeof task !== 'function') throw new TypeError('Poller task must be a function');

    this.task = task;
    this.interval = interval;
    this.hiddenInterval = hiddenInterval;
    this.maxInterval = maxInterval;
    this.onResult = onResult;
    this.onError = onError;

    this.timer = null;
    this.controller = null;
    this.sequence = 0;
    this.errorCount = 0;
    this.running = false;
    this.disposed = false;
    this.inFlight = false;
    this.restartDelay = null;

    this.handleVisibility = this.handleVisibility.bind(this);
    document.addEventListener('visibilitychange', this.handleVisibility);
  }

  start({ immediate = true } = {}) {
    if (this.disposed) return;
    this.stop();
    this.running = true;
    this.errorCount = 0;
    const delay = immediate ? 0 : this.nextDelay();
    if (this.inFlight) this.restartDelay = delay;
    else {
      this.restartDelay = null;
      this.schedule(delay);
    }
  }

  stop() {
    this.running = false;
    this.sequence += 1;
    if (this.timer !== null) {
      clearTimeout(this.timer);
      this.timer = null;
    }
    if (this.controller) {
      this.controller.abort();
    }
  }

  dispose() {
    if (this.disposed) return;
    this.stop();
    this.disposed = true;
    document.removeEventListener('visibilitychange', this.handleVisibility);
  }

  handleVisibility() {
    if (!this.running || this.inFlight) return;
    if (this.timer !== null) clearTimeout(this.timer);
    this.timer = null;
    this.schedule(document.hidden ? this.hiddenInterval : 0);
  }

  nextDelay() {
    if (document.hidden) return this.hiddenInterval;
    if (!this.errorCount) return this.interval;
    return Math.min(this.maxInterval, this.interval * (2 ** this.errorCount));
  }

  schedule(delay) {
    if (!this.running || this.disposed) return;
    if (this.timer !== null) clearTimeout(this.timer);
    this.timer = setTimeout(() => {
      this.timer = null;
      void this.runCycle();
    }, delay);
  }

  async runCycle() {
    if (!this.running || this.disposed || this.inFlight) return;

    const sequence = ++this.sequence;
    const controller = new AbortController();
    this.controller = controller;
    this.inFlight = true;
    let shouldContinue = true;

    try {
      const result = await this.task({ signal: controller.signal, sequence });
      if (!this.isCurrent(sequence, controller)) return;
      this.errorCount = 0;
      shouldContinue = this.onResult(result) !== false;
    } catch (error) {
      if (!this.isCurrent(sequence, controller) || error?.name === 'AbortError') return;
      this.errorCount += 1;
      shouldContinue = this.onError(error) !== false;
    } finally {
      if (this.controller === controller) {
        const currentSequence = this.sequence === sequence;
        this.inFlight = false;
        this.controller = null;
        if (!currentSequence) {
          if (this.running && !this.disposed && this.restartDelay !== null) {
            const delay = this.restartDelay;
            this.restartDelay = null;
            this.schedule(delay);
          }
          return;
        }
        if (!shouldContinue) this.stop();
        else if (this.running && !this.disposed) this.schedule(this.nextDelay());
      }
    }
  }

  isCurrent(sequence, controller) {
    return this.running
      && !this.disposed
      && this.sequence === sequence
      && !controller.signal.aborted;
  }
}
