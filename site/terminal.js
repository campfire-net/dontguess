// terminal.js — Accordion terminal playback from real dontguess demo transcripts
// Curated frames from test/demo/output/*.txt — not fabricated.
(function() {
  'use strict';

  var DEMOS = {
    solo: {
      title: 'Solo operator',
      subtitle: '01-solo-operator.sh',
      source: 'https://github.com/campfire-net/dontguess/blob/main/test/demo/01-solo-operator.sh',
      lines: [
        { type: 'comment', text: '# Operator: init an exchange on a fresh campfire' },
        { type: 'cmd', text: 'cf init' },
        { type: 'output', text: 'Your identity campfire: fc609c1cb410...' },
        { type: 'blank' },
        { type: 'cmd', text: 'dontguess init' },
        { type: 'output', text: 'Exchange initialized' },
        { type: 'output', text: '  campfire: f7b1ccd80322bdd1...' },
        { type: 'output', text: '  operator: 06cda62f30993546...' },
        { type: 'output', text: '  alias:    exchange.dontguess' },
        { type: 'blank' },
        { type: 'comment', text: '# Seller: put cached inference into the exchange' },
        { type: 'cmd', text: 'dontguess put --description "Go HTTP handler for POST JSON" \\' },
        { type: 'cmd', text: '  --content "$(base64 -w0 < handler.go)" --token-cost 2000 --content-type code' },
        { type: 'output', text: 'put message ID: 2b7730c0-b901-4f5e...' },
        { type: 'blank' },
        { type: 'comment', text: '# Start the engine — auto-accept puts at 70% of token cost' },
        { type: 'cmd', text: 'dontguess serve --poll-interval 500ms &' },
        { type: 'output', text: '[exchange] exchange serving' },
        { type: 'output', text: '[exchange]   auto-accept: true (max 100000)' },
        { type: 'output', text: '[exchange] auto-accepted put 2b7730c0: price=1400 (token_cost=2000)' },
        { type: 'blank' },
        { type: 'comment', text: '# Buyer: send a buy request — engine matches against inventory' },
        { type: 'cmd', text: 'dontguess buy --task "Go HTTP handler for POST JSON" --budget 5000' },
        { type: 'output', text: 'buy message ID: ec08a2dd-4f9f...' },
        { type: 'blank' },
        { type: 'comment', text: '# Match arrives — buyer gets the cached result' },
        { type: 'cmd', text: 'dontguess match-results' },
        { type: 'output', text: 'Match messages: 1' },
        { type: 'output', text: '  id=868de439-f2d tags=[\'exchange:match\']' },
        { type: 'output', text: '  results: 1 match(es)' },
        { type: 'output', text: '    entry_id=2b7730c0-b90 confidence=0.50' },
      ]
    },
    multiagent: {
      title: 'Multi-agent flywheel',
      subtitle: '04-multi-agent.sh',
      source: 'https://github.com/campfire-net/dontguess/blob/main/test/demo/04-multi-agent.sh',
      lines: [
        { type: 'comment', text: '# Three distinct identities: operator, seller, buyer' },
        { type: 'cmd', text: 'dontguess init                      # operator' },
        { type: 'output', text: 'Exchange initialized' },
        { type: 'output', text: '  campfire: 25d7e3c0e19eae96...' },
        { type: 'blank' },
        { type: 'comment', text: '# Seller: admit, join, then put 2 items' },
        { type: 'cmd', text: 'cf --cf-home $CF_HOME admit $XCFID $SELLER_PUBKEY' },
        { type: 'output', text: 'Admitted dc51fbf26aba to campfire 25d7e3c0e19e (role: full)' },
        { type: 'cmd', text: 'CF_HOME=$SELLER_CF cf join $XCFID' },
        { type: 'output', text: 'Joined campfire 25d7e3c0e19e' },
        { type: 'blank' },
        { type: 'cmd', text: 'CF_HOME=$SELLER_CF dontguess put --description "Go rate limiter..." \\' },
        { type: 'cmd', text: '  --token-cost 2500 --content-type code' },
        { type: 'output', text: 'put 1 (code) message ID: 12faabfe-02c0...' },
        { type: 'cmd', text: 'CF_HOME=$SELLER_CF dontguess put --description "Terraform VPC analysis..." \\' },
        { type: 'cmd', text: '  --token-cost 4000 --content-type analysis' },
        { type: 'output', text: 'put 2 (analysis) message ID: e9fabdd5-0cd2...' },
        { type: 'blank' },
        { type: 'comment', text: '# Engine auto-accepts both — seller earns scrip' },
        { type: 'cmd', text: 'dontguess serve --poll-interval 500ms &' },
        { type: 'output', text: '[exchange] auto-accepted put 12faabfe: price=1750 (token_cost=2500)' },
        { type: 'output', text: '[exchange] auto-accepted put e9fabdd5: price=2800 (token_cost=4000)' },
        { type: 'output', text: '[exchange] compression assign sent entry=12faabfe bounty=1250' },
        { type: 'blank' },
        { type: 'comment', text: '# Buyer: send buy request — gets match against both items' },
        { type: 'cmd', text: 'CF_HOME=$BUYER_CF dontguess buy --task "rate limiter in Go" --budget 5000' },
        { type: 'blank' },
        { type: 'cmd', text: 'dontguess match-results' },
        { type: 'output', text: 'Match messages: 1' },
        { type: 'output', text: '  results: 2 match(es)' },
        { type: 'output', text: '    entry_id=e9fabdd5-0cd confidence=0.50' },
        { type: 'output', text: '    entry_id=12faabfe-02c confidence=0.50' },
        { type: 'blank' },
        { type: 'comment', text: '# Full message log: 35 messages — 3 identities, 1 exchange' },
        { type: 'cmd', text: 'dontguess messages' },
        { type: 'output', text: '35' },
      ]
    }
  };

  var CHAR_DELAY = 20;
  var LINE_PAUSE = 100;
  var CMD_PAUSE = 350;
  var SECTION_PAUSE = 500;
  var RESTART_DELAY = 4000;

  function TerminalPlayer(el, demo) {
    this.el = el;
    this.demo = demo;
    this.lines = demo.lines;
    this.playing = false;
    this.paused = false;
    this.step = 0;
    this.abortFn = null;
    this.build();
  }

  TerminalPlayer.prototype.build = function() {
    this.el.innerHTML = '';

    // Accordion header
    var header = document.createElement('div');
    header.className = 'term-accordion-header';
    var self = this;
    header.onclick = function() { toggleAccordion(self.el); };

    var arrow = document.createElement('span');
    arrow.className = 'term-accordion-arrow';
    arrow.textContent = '\u25B6';

    var title = document.createElement('span');
    title.className = 'term-accordion-title';
    title.textContent = this.demo.title;

    var subtitle = document.createElement('a');
    subtitle.className = 'term-accordion-subtitle';
    subtitle.href = this.demo.source;
    subtitle.target = '_blank';
    subtitle.rel = 'noopener';
    subtitle.textContent = this.demo.subtitle;
    subtitle.onclick = function(e) { e.stopPropagation(); };

    header.appendChild(arrow);
    header.appendChild(title);
    header.appendChild(subtitle);
    this.el.appendChild(header);

    // Terminal body (collapsed by default)
    var body = document.createElement('div');
    body.className = 'term-accordion-body';

    var screen = document.createElement('div');
    screen.className = 'term-screen';
    this.screen = screen;

    var output = document.createElement('div');
    output.className = 'term-output';
    this.output = output;

    var cursor = document.createElement('span');
    cursor.className = 'term-cursor';
    cursor.textContent = '\u2588';

    screen.appendChild(output);
    screen.appendChild(cursor);
    body.appendChild(screen);
    this.el.appendChild(body);
  };

  TerminalPlayer.prototype.play = function() {
    if (this.playing) return;
    this.playing = true;
    this.paused = false;
    this.step = 0;
    this.output.innerHTML = '';
    this.playStep();
  };

  TerminalPlayer.prototype.playStep = function() {
    if (!this.playing) return;
    if (this.step >= this.lines.length) {
      var self = this;
      var t = setTimeout(function() {
        self.playing = false;
        self.play();
      }, RESTART_DELAY);
      this.abortFn = function() { clearTimeout(t); };
      return;
    }

    var line = this.lines[this.step];
    this.step++;
    var self = this;

    if (line.type === 'blank') {
      this.appendLine('', 'term-blank');
      setTimeout(function() { self.playStep(); }, LINE_PAUSE);
    } else if (line.type === 'comment') {
      this.appendLine(line.text, 'term-comment');
      setTimeout(function() { self.playStep(); }, LINE_PAUSE);
    } else if (line.type === 'output') {
      this.appendLine(line.text, 'term-out');
      setTimeout(function() { self.playStep(); }, LINE_PAUSE);
    } else if (line.type === 'cmd' || line.type === 'cmd-cont') {
      var prefix = line.type === 'cmd' ? '$ ' : '  ';
      this.typeCmd(prefix, line.text, 'term-cmd', function() {
        setTimeout(function() { self.playStep(); }, CMD_PAUSE);
      });
    }
  };

  TerminalPlayer.prototype.typeCmd = function(prefix, text, cls, done) {
    var row = document.createElement('div');
    row.className = cls;
    var span = document.createElement('span');
    span.textContent = prefix;
    row.appendChild(span);
    this.output.appendChild(row);
    this.scrollToBottom();

    var i = 0;
    var self = this;
    function next() {
      if (!self.playing) return;
      if (i >= text.length) { done(); return; }
      span.textContent = prefix + text.slice(0, ++i);
      self.scrollToBottom();
      var t = setTimeout(next, CHAR_DELAY);
      self.abortFn = function() { clearTimeout(t); };
    }
    next();
  };

  TerminalPlayer.prototype.appendLine = function(text, cls) {
    var row = document.createElement('div');
    row.className = cls;
    row.textContent = text;
    this.output.appendChild(row);
    this.scrollToBottom();
  };

  TerminalPlayer.prototype.scrollToBottom = function() {
    this.screen.scrollTop = this.screen.scrollHeight;
  };

  TerminalPlayer.prototype.stop = function() {
    this.playing = false;
    if (this.abortFn) { this.abortFn(); this.abortFn = null; }
  };

  function toggleAccordion(el) {
    var body = el.querySelector('.term-accordion-body');
    var arrow = el.querySelector('.term-accordion-arrow');
    var player = el._termPlayer;
    if (!body) return;

    var isOpen = body.classList.contains('open');
    if (isOpen) {
      body.classList.remove('open');
      arrow.textContent = '\u25B6';
      if (player) player.stop();
    } else {
      body.classList.add('open');
      arrow.textContent = '\u25BC';
      if (player) { setTimeout(function() { player.play(); }, 200); }
    }
  }

  function init() {
    var containers = document.querySelectorAll('[data-terminal-demo]');
    for (var i = 0; i < containers.length; i++) {
      var el = containers[i];
      var key = el.getAttribute('data-terminal-demo');
      var demo = DEMOS[key];
      if (!demo) continue;
      el.className = 'term-accordion';
      var player = new TerminalPlayer(el, demo);
      el._termPlayer = player;
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
