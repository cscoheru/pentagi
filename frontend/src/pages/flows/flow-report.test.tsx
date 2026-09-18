import { render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

const queryResult = vi.hoisted(() => ({
    current: { data: undefined, error: undefined, loading: false } as Record<string, unknown>,
}));

const summaryResult = vi.hoisted(() => ({
    current: { data: undefined, error: undefined, loading: false } as Record<string, unknown>,
}));

const reportResult = vi.hoisted(() => ({
    current: { data: undefined, error: undefined, loading: false } as Record<string, unknown>,
}));

const summaryDoc = vi.hoisted(() => ({ __summary: true }));
const reportDoc = vi.hoisted(() => ({ __report: true }));

vi.mock('@apollo/client/react', () => ({
    skipToken: Symbol('skipToken'),
    useQuery: (doc: unknown) => {
        if (doc === summaryDoc) {
            return summaryResult.current;
        }

        if (doc === reportDoc) {
            return reportResult.current;
        }

        return queryResult.current;
    },
}));

vi.mock('@/graphql/types', () => ({
    AgentReportDocument: reportDoc,
    FlowSummaryDocument: summaryDoc,
}));

vi.mock('react-router-dom', () => ({
    useParams: () => ({ flowId: '42' }),
    useSearchParams: () => [new URLSearchParams()],
}));

vi.mock('@/components/shared/markdown', () => ({
    default: ({ children }: { children: string }) => <div data-testid="markdown">{children}</div>,
}));

vi.mock('@/lib/report', () => ({
    generateFileName: () => 'report',
    generatePDFFromMarkdown: vi.fn().mockResolvedValue(undefined),
}));

const { default: FlowReport } = await import('./flow-report');

const resetResults = () => {
    summaryResult.current = { data: undefined, error: undefined, loading: false };
    reportResult.current = { data: undefined, error: undefined, loading: false };
};

describe('FlowReport load states', () => {
    it('renders the agent-written report when a partial error arrives alongside a usable flow', () => {
        resetResults();
        summaryResult.current = {
            data: { flow: { id: '42', title: 'Recon' } },
            error: new Error('failed to fetch other stuff'),
            loading: false,
        };
        reportResult.current = {
            data: { agentReport: '# Final Report\n\nFrom pentAGI agent.' },
            error: undefined,
            loading: false,
        };

        render(<FlowReport />);

        expect(screen.getByTestId('markdown')).toHaveTextContent('# Final Report From pentAGI agent.');
        expect(screen.queryByText('Failed to load flow data')).not.toBeInTheDocument();
    });

    it('reports a failure when the flow itself is missing', () => {
        resetResults();
        summaryResult.current = { data: { flow: null }, error: new Error('boom'), loading: false };
        reportResult.current = {
            data: { agentReport: 'should not be shown' },
            error: undefined,
            loading: false,
        };

        render(<FlowReport />);

        expect(screen.getByText('Failed to load flow data')).toBeInTheDocument();
    });

    it('renders the agent-written report alongside a successful flow summary', () => {
        resetResults();
        summaryResult.current = {
            data: { flow: { id: '42', title: 'Recon' } },
            error: undefined,
            loading: false,
        };
        reportResult.current = {
            data: { agentReport: '# Puer.im Assessment\n\nReal content here.' },
            error: undefined,
            loading: false,
        };

        render(<FlowReport />);

        expect(screen.getByTestId('markdown')).toHaveTextContent('# Puer.im Assessment Real content here.');
    });
});
