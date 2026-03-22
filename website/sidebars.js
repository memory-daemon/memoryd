/** @type {import('@docusaurus/plugin-content-docs').SidebarsConfig} */
const sidebars = {
  docs: [
    'intro',
    'getting-started',
    'team-knowledge-hub',
    {
      type: 'category',
      label: 'Connecting Your Tools',
      items: [
        'agents/proxy-mode',
        'agents/mcp-server',
        'agents/read-only-mode',
      ],
    },
    {
      type: 'category',
      label: 'How It Works',
      items: [
        'how-it-works/read-path',
        'how-it-works/write-path',
        'how-it-works/quality-loop',
        'how-it-works/hybrid-search',
        'how-it-works/architecture',
      ],
    },
    'configuration',
  ],
};

export default sidebars;
