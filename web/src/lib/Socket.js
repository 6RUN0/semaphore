import Listenable from '@/lib/Listenable';
import FakeWebSocket from './FakeWebSocket';

// Long enough to let a restarting server finish, short enough that a user
// watching a running task does not notice the gap.
const RECONNECT_DELAY = 2000;

export default class Socket extends Listenable {
  constructor(websocketCreator) {
    super();
    this.websocketCreator = websocketCreator;
    this.sessionActive = false;
    this.wantStart = false;
    this.ws = null;
  }

  /**
   * Sets whether the user session is active.
   * @param {boolean} isActive - Whether the user session is active
   */
  setSessionActive(isActive) {
    this.sessionActive = isActive;
    this.syncConnection();
  }

  start() {
    this.wantStart = true;
    this.syncConnection();
  }

  stop() {
    this.wantStart = false;
    this.syncConnection();
  }

  isRunning() {
    return this.ws != null;
  }

  /**
   * Brings the connection in line with what is currently wanted: none unless
   * start() was called, a real one while the session is active, and a fake one
   * otherwise.
   *
   * Every state change goes through here rather than acting on its own, because
   * the wanted connection depends on both flags at once: the socket may be
   * started before the session is confirmed, and the session may end and come
   * back while the caller still wants to be connected.
   */
  syncConnection() {
    if (!this.wantStart) {
      this.closeConnection();
      return;
    }

    // Connecting without a session fails the handshake and logs an error, so a
    // socket that does nothing stands in until the user is known.
    const wantsFake = !this.sessionActive;

    if (this.ws != null && (this.ws instanceof FakeWebSocket) === wantsFake) {
      return;
    }

    this.closeConnection();

    if (wantsFake) {
      this.ws = new FakeWebSocket();
    } else {
      this.openConnection();
    }
  }

  openConnection() {
    const connection = this.websocketCreator();
    this.ws = connection;

    connection.onclose = () => {
      // A connection that has already been closed or replaced must not touch
      // the current one, and its close is not a reason to reconnect.
      if (this.ws !== connection) {
        return;
      }
      this.ws = null;
      // Reconnecting through syncConnection rather than directly, because the
      // session may well be gone by the time the delay is over.
      setTimeout(() => this.syncConnection(), RECONNECT_DELAY);
    };

    connection.onmessage = ({ data }) => {
      this.callListeners(JSON.parse(data));
    };
  }

  closeConnection() {
    if (this.ws == null) {
      return;
    }
    const connection = this.ws;
    this.ws = null;
    connection.close();
  }
}
