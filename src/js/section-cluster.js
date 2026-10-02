/* Section 4: cluster and nodes. Not built yet; it shows what it will cover. */
(function () {
  'use strict';

  SP.sections.register({
    id: 'cluster', number: 4, title: 'Cluster and nodes', placeholder: true,
    render: function (host) {
      host.innerHTML = '<p class="placeholder">Node types, capacity and what they cost.</p>';
    }
  });
})();
