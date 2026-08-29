import { expect } from 'chai';
import Socket from '@/lib/Socket';

// The real socket is whatever the creator returns, so a plain object with the
// two handlers Socket assigns is enough to drive every branch from the test.
function createSocket() {
  const opened = [];

  const socket = new Socket(() => {
    const connection = { closed: false, close() { this.closed = true; } };
    opened.push(connection);
    return connection;
  });

  return { socket, opened };
}

describe('Socket', () => {
  // Opening the connection before the user is known would fail the handshake
  // and log an error on every anonymous page, which is what the fake socket
  // exists to prevent.
  it('leaves the connection unopened while the session is inactive', () => {
    const { socket, opened } = createSocket();

    socket.start();

    expect(opened).to.have.lengthOf(0);
    expect(socket.isRunning()).to.equal(true);
  });

  it('opens the connection when the session is active', () => {
    const { socket, opened } = createSocket();

    socket.setSessionActive(true);
    socket.start();

    expect(opened).to.have.lengthOf(1);
    expect(socket.isRunning()).to.equal(true);
  });

  it('starts only one connection however often it is started', () => {
    const { socket, opened } = createSocket();

    socket.setSessionActive(true);
    socket.start();
    socket.start();

    expect(opened).to.have.lengthOf(1);
  });

  it('hands parsed messages to every listener', () => {
    const { socket, opened } = createSocket();
    const received = [];

    socket.setSessionActive(true);
    socket.start();
    socket.addListener((data) => received.push(data));
    socket.addListener((data) => received.push(data));

    opened[0].onmessage({ data: '{"type":"update"}' });

    expect(received).to.deep.equal([{ type: 'update' }, { type: 'update' }]);
  });

  it('stops delivering to a removed listener', () => {
    const { socket, opened } = createSocket();
    const received = [];

    socket.setSessionActive(true);
    socket.start();
    const id = socket.addListener((data) => received.push(data));
    socket.removeListener(id);

    opened[0].onmessage({ data: '{"type":"update"}' });

    expect(received).to.have.lengthOf(0);
  });

  // start() may run before the session is confirmed. The fake connection has to
  // give way to a real one as soon as the user is known, or the tab keeps a
  // socket that never delivers anything.
  it('replaces the fake connection once the session becomes active', () => {
    const { socket, opened } = createSocket();

    socket.start();
    socket.setSessionActive(true);

    expect(opened).to.have.lengthOf(1);
    expect(socket.isRunning()).to.equal(true);
  });

  // The real connection cannot survive the session, but the fake one keeps the
  // place so that nothing else tries to reopen it.
  it('closes the real connection when the session ends', () => {
    const { socket, opened } = createSocket();

    socket.setSessionActive(true);
    socket.start();
    socket.setSessionActive(false);

    expect(opened[0].closed).to.equal(true);
    expect(opened).to.have.lengthOf(1);
    expect(socket.isRunning()).to.equal(true);
  });

  // Losing the session must not erase the request to be connected, otherwise
  // the socket stays down for good once the session returns.
  it('reopens the connection when the session comes back', () => {
    const { socket, opened } = createSocket();

    socket.setSessionActive(true);
    socket.start();
    socket.setSessionActive(false);
    socket.setSessionActive(true);

    expect(opened).to.have.lengthOf(2);
    expect(opened[0].closed).to.equal(true);
    expect(opened[1].closed).to.equal(false);
  });

  it('closes the connection when it is stopped', () => {
    const { socket, opened } = createSocket();

    socket.setSessionActive(true);
    socket.start();
    socket.stop();

    expect(opened[0].closed).to.equal(true);
    expect(socket.isRunning()).to.equal(false);
  });

  // A browser reports the close of a socket after close() returns, by which
  // time a new one may already be in place. The late report belongs to the old
  // connection and must leave the new one alone.
  it('keeps the new connection when the previous one reports its close', () => {
    const { socket, opened } = createSocket();

    socket.setSessionActive(true);
    socket.start();
    socket.stop();
    socket.start();

    opened[0].onclose();

    expect(opened).to.have.lengthOf(2);
    expect(opened[1].closed).to.equal(false);
    expect(socket.isRunning()).to.equal(true);
  });

  // App.vue restarts the socket whenever it finds it not running, so a
  // connection dropped by the server has to stop counting as running.
  it('forgets the connection when the server drops it', () => {
    const { socket, opened } = createSocket();

    socket.setSessionActive(true);
    socket.start();
    opened[0].onclose();

    expect(socket.isRunning()).to.equal(false);

    // The reconnect is scheduled on a timer that reads this state when it
    // fires; stopping here keeps it from outliving the test.
    socket.stop();
  });
});
