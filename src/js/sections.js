/* Registry of the report's numbered sections. Each section-*.js file
 * registers one; the page lays them out in number order and lists them in
 * the section bar.
 *
 *   SP.sections.register({
 *     id: 'money',              // the hash and element id, one of SP.state.SECTIONS
 *     number: 2,
 *     title: 'Where the money is',
 *     render: function (host, ctx) {},
 *     resize: function (host, ctx) {}   // optional: redraw after the window is resized
 *   });
 *
 * render is called with the section's element after a file loads and again
 * whenever the state changes, unless only the scroll position did. ctx holds:
 *   data     the prepared file (model.prepare), the same object until another file opens
 *   result   figures under the current levers (model.compute)
 *   state    the shared state (SP.state), read only
 *   changed  the state keys that changed since the last render, or null after a load
 *   set      set(patch) changes the state; the page recomputes and re-renders
 *   go       go(sectionId) scrolls to a section, opening the details for any but the summary
 */
(function () {
  'use strict';

  var list = [];

  function register(section) {
    list.push(section);
    list.sort(function (a, b) { return a.number - b.number; });
  }

  (self.SP = self.SP || {}).sections = { register: register, list: list };
})();
